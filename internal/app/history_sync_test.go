package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestHistorySnapshotDeltaAndOldRecordUpdates(t *testing.T) {
	s, err := OpenStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	initial := s.syncHistory("")
	if initial.Mode != "full" || len(initial.Messages) != 0 {
		t.Fatal("initial snapshot")
	}
	message := Message{ID: "old-message", Direction: DirectionIncoming, Text: "fixture", CreatedAt: time.Unix(1, 0)}
	if err := s.SaveMessage(message); err != nil {
		t.Fatal(err)
	}
	first := s.syncHistory(initial.Cursor)
	if first.Mode != "delta" || first.BaseCursor != initial.Cursor || len(first.Messages) != 1 {
		t.Fatal("missing delta")
	}
	message.DeliveryState = "delivered"
	if err := s.SaveMessage(message); err != nil {
		t.Fatal(err)
	}
	second := s.syncHistory(first.Cursor)
	var delivered Message
	if len(second.Messages) != 1 {
		t.Fatal("old row update missing")
	}
	if err := json.Unmarshal(second.Messages[0], &delivered); err != nil || delivered.DeliveryState != "delivered" {
		t.Fatal("receipt missing")
	}
	noop := s.syncHistory(second.Cursor)
	encoded, _ := json.Marshal(noop)
	if noop.Cursor != second.Cursor || len(noop.Messages) != 0 || len(encoded) > 500 {
		t.Fatal("unchanged history must be compact")
	}
	if err := s.SaveMessage(message); err != nil {
		t.Fatal(err)
	}
	if s.syncHistory(second.Cursor).Cursor != second.Cursor {
		t.Fatal("identical save advanced cursor")
	}
}

func TestHistoryInvalidExpiredRestartAndDeletion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, _ := OpenStore(path)
	initial := s.syncHistory("")
	for index := 0; index < historyChangeLimit+1; index++ {
		if err := s.SaveMessage(Message{ID: "one", Text: fmt.Sprint(index)}); err != nil {
			t.Fatal(err)
		}
	}
	if got := s.syncHistory(initial.Cursor); got.Mode != "full" || got.Reason != "cursor_invalid" {
		t.Fatal("expired cursor accepted")
	}
	current := s.syncHistory("")
	for _, cursor := range []string{"bogus", strings.Split(current.Cursor, ":")[0] + ":999999", current.Cursor + ":0"} {
		if got := s.syncHistory(cursor); got.Mode != "full" {
			t.Fatal("invalid cursor accepted")
		}
	}
	s.mu.Lock()
	delete(s.messages, "one")
	err := s.flushLocked()
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	deleted := s.syncHistory(current.Cursor)
	if len(deleted.DeletedMessages) != 1 || len(deleted.Messages) != 0 {
		t.Fatal("missing tombstone")
	}
	reopened, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.syncHistory(deleted.Cursor); got.Mode != "full" {
		t.Fatal("restart reused generation")
	}
}

func TestHistoryByteBudgetAndDurableFailure(t *testing.T) {
	s, _ := OpenStore(filepath.Join(t.TempDir(), "state.json"))
	initial := s.syncHistory("")
	if err := s.SaveMessage(Message{ID: "large", Text: strings.Repeat("x", historyChangeBytes+1)}); err != nil {
		t.Fatal(err)
	}
	if got := s.syncHistory(initial.Cursor); got.Mode != "full" {
		t.Fatal("oversized delta retained")
	}
	current := s.syncHistory("")
	s.path = t.TempDir() // Renaming a file over a directory must fail.
	if s.SaveMessage(Message{ID: "failed", Text: "fixture"}) == nil {
		t.Fatal("expected persistence failure")
	}
	if got := s.syncHistory(current.Cursor); got.Cursor != current.Cursor || len(got.Messages) != 0 {
		t.Fatal("failed write published")
	}
}

func TestHistoryPushResumeAndExplicitCheck(t *testing.T) {
	a, server := newIntegrationTestServer(t)
	socket, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/api/v1/history/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer socket.Close()
	read := func() historySync {
		t.Helper()
		_ = socket.SetReadDeadline(time.Now().Add(2 * time.Second))
		var value historySync
		if err := socket.ReadJSON(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	initial := read()
	if err := a.store.SaveCall(Call{ID: "fixture-call", State: "ringing", StartedAt: time.Unix(2, 0)}); err != nil {
		t.Fatal(err)
	}
	pushed := read()
	if pushed.Mode != "delta" || pushed.BaseCursor != initial.Cursor || len(pushed.Calls) != 1 {
		t.Fatal("not pushed incrementally")
	}
	if err := socket.WriteJSON(map[string]string{"type": "sync", "cursor": pushed.Cursor}); err != nil {
		t.Fatal(err)
	}
	if checked := read(); checked.Cursor != pushed.Cursor || len(checked.Calls) != 0 {
		t.Fatal("check downloaded history")
	}
	if err := socket.WriteJSON(map[string]string{"type": "sync", "cursor": initial.Cursor}); err != nil {
		t.Fatal(err)
	}
	if replay := read(); len(replay.Calls) != 1 {
		t.Fatal("missing replay")
	}
}

func TestHistoryAuthenticationAndNoStore(t *testing.T) {
	a, _ := newIntegrationTestServer(t)
	a.cfg.APIKey = "fixture-key"
	a.mux = http.NewServeMux()
	a.routes()
	for _, path := range []string{"/api/v1/history", "/api/v1/history/ws"} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		recorder := httptest.NewRecorder()
		a.Handler().ServeHTTP(recorder, request)
		if recorder.Code != http.StatusUnauthorized {
			t.Fatal("history leaked without authorization")
		}
	}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/history", nil)
	request.Header.Set("Authorization", "Bearer fixture-key")
	recorder := httptest.NewRecorder()
	a.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("snapshot contract")
	}
}

func TestHistoryContactRenameAndTerminalCallUpdates(t *testing.T) {
	s, _ := OpenStore(filepath.Join(t.TempDir(), "state.json"))
	if err := s.SaveMessage(Message{ID: "message", Direction: DirectionIncoming, From: "+12025550123"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveCall(Call{ID: "call", TraceID: "fixture-trace", Peer: "+12025550123", State: "active"}); err != nil {
		t.Fatal(err)
	}
	initial := s.syncHistory("")
	_, err := s.ReplaceContactSource("fixture-client", "Fixture", "US", 1, []ContactSourceRecord{{
		SourceContactID: "contact", Names: []string{"Fixture name"}, Numbers: []ContactNumber{{Original: "+12025550123"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	delta := s.syncHistory(initial.Cursor)
	if len(delta.Messages) != 1 || len(delta.Calls) != 1 || delta.ContactsVersion == initial.ContactsVersion {
		t.Fatal("contact update not propagated")
	}
	var call Call
	if err := json.Unmarshal(delta.Calls[0], &call); err != nil || call.ContactName != "Fixture name" {
		t.Fatal("missing derived name")
	}
	ended := time.Now().UTC()
	call.State = "ended"
	call.EndedAt = &ended
	if err := s.SaveCall(call); err != nil {
		t.Fatal(err)
	}
	if got := s.syncHistory(delta.Cursor); len(got.Calls) != 1 || len(got.Messages) != 0 {
		t.Fatal("call end sent unrelated history")
	}
}

func TestActiveCallProbeDoesNotDownloadHistory(t *testing.T) {
	a, _ := newIntegrationTestServer(t)
	call := Call{ID: "fixture-call", TraceID: "fixture-trace", State: "active"}
	if err := a.store.SaveCall(call); err != nil {
		t.Fatal(err)
	}
	for _, active := range []bool{true, false} {
		if !active {
			now := time.Now().UTC()
			call.EndedAt = &now
			if err := a.store.SaveCall(call); err != nil {
				t.Fatal(err)
			}
		}
		recorder := httptest.NewRecorder()
		a.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/calls/active?trace_id=fixture-trace", nil))
		var result map[string]bool
		if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if result["active"] != active || len(result) != 1 || recorder.Body.Len() > 32 {
			t.Fatal("noncompact or incorrect active probe")
		}
	}
}
