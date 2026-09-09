package app

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestMergedContactsNormalizeByRegionAndRetainProvenance(t *testing.T) {
	records := []ContactSourceRecord{
		{SourceClientID: "phone-a", SourceClientName: "OnePlus", SourceContactID: "1", Region: "ES", Names: []string{"Alice"}, Numbers: []ContactNumber{{Original: "612 34 56 78", Label: "Mobile"}}},
		{SourceClientID: "phone-b", SourceClientName: "Pixel", SourceContactID: "9", Region: "ES", Names: []string{"Alice García"}, Numbers: []ContactNumber{{Original: "+34 612 345 678"}}},
	}
	contacts := mergedContacts(records, "")
	if len(contacts) != 1 {
		t.Fatalf("contacts = %#v, want one merged phone identity", contacts)
	}
	contact := contacts[0]
	if len(contact.Sources) != 2 || len(contact.Names) != 2 || len(contact.Numbers) != 1 {
		t.Fatalf("merged contact = %#v", contact)
	}
	if contact.Numbers[0].Normalized != "+34612345678" {
		t.Fatalf("normalized number = %q", contact.Numbers[0].Normalized)
	}
	if len(mergedContacts(records, "garcía")) != 1 || len(mergedContacts(records, "612345")) != 1 {
		t.Fatal("contact was not searchable by alternative name and number")
	}
}

func TestMergedContactsNeverMergeOnNameAlone(t *testing.T) {
	records := []ContactSourceRecord{
		{SourceClientID: "a", SourceContactID: "1", Region: "ES", Names: []string{"Shared name"}, Numbers: []ContactNumber{{Original: "+34600111222"}}},
		{SourceClientID: "b", SourceContactID: "2", Region: "ES", Names: []string{"Shared name"}, Numbers: []ContactNumber{{Original: "+34600999888"}}},
	}
	if contacts := mergedContacts(records, ""); len(contacts) != 2 {
		t.Fatalf("contacts = %#v, want two distinct phone identities", contacts)
	}
	if contacts := mergedContacts(records, "missing name"); len(contacts) != 0 {
		t.Fatalf("unrelated name search returned %#v", contacts)
	}
}

func TestReplacingOneContactSourcePreservesOtherDeviceContribution(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	record := func(id string) []ContactSourceRecord {
		return []ContactSourceRecord{{SourceContactID: id, Names: []string{"Alice"}, Numbers: []ContactNumber{{Original: "+34612345678"}}}}
	}
	if _, err := store.ReplaceContactSource("phone-a", "OnePlus", "ES", record("1")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReplaceContactSource("phone-b", "Pixel", "ES", record("9")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReplaceContactSource("phone-a", "OnePlus", "ES", nil); err != nil {
		t.Fatal(err)
	}
	contacts := mergedContacts(store.ContactSources(), "")
	if len(contacts) != 1 || len(contacts[0].Sources) != 1 || contacts[0].Sources[0].ClientID != "phone-b" {
		t.Fatalf("remaining contacts = %#v", contacts)
	}
}

func TestReplacingIdenticalContactSourceIsANoOp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	records := []ContactSourceRecord{{SourceContactID: "42", Names: []string{"Alice"}, Numbers: []ContactNumber{{Original: "+34612345678"}}}}
	changed, err := store.ReplaceContactSource("phone-a", "OnePlus", "ES", records)
	if err != nil || !changed {
		t.Fatalf("first replace changed=%v err=%v", changed, err)
	}
	first := store.ContactSources()[0].UpdatedAt
	changed, err = store.ReplaceContactSource("phone-a", "OnePlus", "ES", records)
	if err != nil || changed {
		t.Fatalf("identical replace changed=%v err=%v", changed, err)
	}
	if second := store.ContactSources()[0].UpdatedAt; !second.Equal(first) {
		t.Fatalf("no-op replace changed timestamp from %s to %s", first, second)
	}
	reopened, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if contacts := mergedContacts(reopened.ContactSources(), "Alice"); len(contacts) != 1 || contacts[0].Sources[0].ClientName != "OnePlus" {
		t.Fatalf("persisted contacts = %#v", contacts)
	}
}

func TestAmbiguousNumbersArePreservedWithoutCrossDeviceMerge(t *testing.T) {
	records := []ContactSourceRecord{
		{SourceClientID: "a", SourceContactID: "1", Names: []string{"Desk"}, Numbers: []ContactNumber{{Original: "extension 123"}}},
		{SourceClientID: "b", SourceContactID: "2", Names: []string{"Desk"}, Numbers: []ContactNumber{{Original: "extension 123"}}},
	}
	contacts := mergedContacts(records, "")
	if len(contacts) != 2 || contacts[0].Numbers[0].Original != "extension 123" {
		t.Fatalf("ambiguous contacts = %#v", contacts)
	}
}

func TestContactSyncAPIIsRepeatableAndEnrichesSharedHistory(t *testing.T) {
	application, err := New(Config{
		DataFile:  filepath.Join(t.TempDir(), "state.json"),
		Telephony: TelephonyConfig{Type: "memory"},
		SIMAuth:   SIMAuthConfig{Type: "disabled"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"client_name":"OnePlus","region":"ES","contacts":[{"source_contact_id":"42","names":["Alice","Alicia"],"numbers":[{"original":"612 34 56 78","normalized":"","label":"Mobile"}]}]}`)
	for range 2 {
		request := httptest.NewRequest(http.MethodPut, "/api/v1/contacts/sources/phone-one", bytes.NewReader(body))
		response := httptest.NewRecorder()
		application.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusNoContent {
			t.Fatalf("sync status = %d: %s", response.Code, response.Body.String())
		}
	}
	now := time.Now().UTC()
	if err := application.store.SaveMessage(Message{ID: "message-one", Direction: DirectionIncoming, From: "+34612345678", State: "received", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/messages", nil)
	response := httptest.NewRecorder()
	application.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("messages status = %d: %s", response.Code, response.Body.String())
	}
	var payload struct {
		Messages []Message `json:"messages"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Messages) != 1 || payload.Messages[0].ContactName != "Alice" || payload.Messages[0].ContactID == "" {
		t.Fatalf("enriched message = %#v", payload.Messages)
	}
	if err := application.store.SaveCall(Call{ID: "call-one", Direction: DirectionIncoming, Peer: "+34612345678", State: "completed", StartedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	request = httptest.NewRequest(http.MethodGet, "/api/v1/calls", nil)
	response = httptest.NewRecorder()
	application.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("calls status = %d: %s", response.Code, response.Body.String())
	}
	var callsPayload struct {
		Calls []Call `json:"calls"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &callsPayload); err != nil {
		t.Fatal(err)
	}
	if len(callsPayload.Calls) != 1 || callsPayload.Calls[0].ContactName != "Alice" || callsPayload.Calls[0].ContactID == "" {
		t.Fatalf("enriched call = %#v", callsPayload.Calls)
	}
}
