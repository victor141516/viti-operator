package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type conferenceControlFixture struct {
	MemoryGateway
	mu          sync.Mutex
	dropped     []string
	conferences []string
	dropErrors  []error
	endErrors   []error
}

func (g *conferenceControlFixture) DropConferenceLeg(_ context.Context, channel string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.dropped = append(g.dropped, channel)
	if len(g.dropErrors) > 0 {
		err := g.dropErrors[0]
		g.dropErrors = g.dropErrors[1:]
		return err
	}
	return nil
}

func (g *conferenceControlFixture) EndConference(_ context.Context, conference string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.conferences = append(g.conferences, conference)
	if len(g.endErrors) > 0 {
		err := g.endErrors[0]
		g.endErrors = g.endErrors[1:]
		return err
	}
	return nil
}

func sessionTestCall() Call {
	return Call{ID: "call-public", Instance: "engine-1", EngineCallID: "asterisk-42", TraceID: "trace-42", State: "active"}
}

func TestUnexpectedLastLegLossRetainsLogicalParticipant(t *testing.T) {
	r := newCallSessionRegistry()
	call := sessionTestCall()
	now := time.Now().UTC()
	r.joined(call, "phone-a", "phone-a", "leg-1", "PJSIP/web-1", now)
	r.left(call, "phone-a", "phone-a", "leg-1", now.Add(time.Second))

	if _, err := r.beginLeave(call, "phone-a", "leg-1", "leave-1"); err != nil {
		t.Fatalf("disconnected logical participant should remain explicitly leaveable: %v", err)
	}
}

func TestReconnectSupersedesOldLegAndLateLeaveCannotDisconnectNewLeg(t *testing.T) {
	r := newCallSessionRegistry()
	call := sessionTestCall()
	now := time.Now().UTC()
	r.joined(call, "phone-a", "phone-a", "leg-1", "PJSIP/web-1", now)
	if got := r.joined(call, "phone-a", "phone-a", "leg-2", "PJSIP/web-2", now.Add(time.Second)); got.supersededChannel != "PJSIP/web-1" {
		t.Fatalf("superseded channel = %q, want old channel", got.supersededChannel)
	}
	r.left(call, "phone-a", "phone-a", "leg-1", now.Add(2*time.Second))

	operation, err := r.beginLeave(call, "phone-a", "leg-2", "leave-2")
	if err != nil {
		t.Fatal(err)
	}
	if operation.channel != "PJSIP/web-2" || !operation.endCarrier {
		t.Fatalf("leave used stale state: %+v", operation)
	}
}

func TestLateJoinCannotResurrectExplicitlyDepartedGeneration(t *testing.T) {
	r := newCallSessionRegistry()
	call := sessionTestCall()
	now := time.Now().UTC().Add(-time.Minute)
	r.joined(call, "phone-a", "phone-a", "leg-1", "PJSIP/web-1", now)
	if _, err := r.beginLeave(call, "phone-a", "leg-1", "leave-1"); err != nil {
		t.Fatal(err)
	}

	late := r.joined(call, "phone-a", "phone-a", "leg-1", "PJSIP/web-late", now.Add(time.Second))
	if late.rejectedChannel != "PJSIP/web-late" {
		t.Fatalf("late departed generation was not rejected: %+v", late)
	}
	if _, err := r.beginLeave(call, "phone-a", "leg-1", "leave-again"); !errors.Is(err, errCallParticipantNotFound) {
		t.Fatalf("late join resurrected logical membership: %v", err)
	}
}

func TestNewGenerationCanJoinAfterExplicitDeparture(t *testing.T) {
	r := newCallSessionRegistry()
	call := sessionTestCall()
	now := time.Now().UTC().Add(-time.Minute)
	r.joined(call, "phone-a", "phone-a", "leg-1", "PJSIP/web-1", now)
	if _, err := r.beginLeave(call, "phone-a", "leg-1", "leave-1"); err != nil {
		t.Fatal(err)
	}

	joined := r.joined(call, "phone-a", "phone-a", "leg-2", "PJSIP/web-2", time.Now().UTC().Add(time.Second))
	if joined.rejectedChannel != "" {
		t.Fatalf("newer generation was rejected: %+v", joined)
	}
	if _, err := r.beginLeave(call, "phone-a", "leg-2", "leave-2"); err != nil {
		t.Fatalf("newer generation was not registered: %v", err)
	}
}

func TestExplicitLeaveOnlyEndsCarrierAfterLastLogicalParticipant(t *testing.T) {
	r := newCallSessionRegistry()
	call := sessionTestCall()
	now := time.Now().UTC()
	r.joined(call, "web-a", "web-a", "leg-a", "PJSIP/a", now)
	r.joined(call, "phone-b", "phone-b", "leg-b", "PJSIP/b", now)
	r.left(call, "phone-b", "phone-b", "leg-b", now.Add(time.Second))

	first, err := r.beginLeave(call, "web-a", "leg-a", "leave-a")
	if err != nil {
		t.Fatal(err)
	}
	if first.endCarrier {
		t.Fatal("an unexpectedly disconnected logical participant must keep the carrier call alive")
	}
	last, err := r.beginLeave(call, "phone-b", "leg-b", "leave-b")
	if err != nil {
		t.Fatal(err)
	}
	if !last.endCarrier {
		t.Fatal("the last explicit logical departure must end the carrier call")
	}
}

func TestLeaveOperationIsIdempotentAndCannotBeReused(t *testing.T) {
	r := newCallSessionRegistry()
	call := sessionTestCall()
	r.joined(call, "web-a", "web-a", "leg-a", "PJSIP/a", time.Now().UTC())
	first, err := r.beginLeave(call, "web-a", "leg-a", "operation-1")
	if err != nil {
		t.Fatal(err)
	}
	second, err := r.beginLeave(call, "web-a", "leg-a", "operation-1")
	if err != nil || first != second {
		t.Fatalf("retry did not return the same operation: first=%+v second=%+v err=%v", first, second, err)
	}
	other := call
	other.EngineCallID = "asterisk-new"
	if _, err := r.beginLeave(other, "web-a", "leg-a", "operation-1"); !errors.Is(err, errLeaveOperationConflict) {
		t.Fatalf("operation reuse error = %v", err)
	}
}

func TestInboundProvisionalIdentityCanBeClaimed(t *testing.T) {
	r := newCallSessionRegistry()
	call := sessionTestCall()
	r.joined(call, "", "asterisk-participant", "leg-in", "PJSIP/in", time.Now().UTC())
	if !r.claim(call.TraceID, "asterisk-participant", "phone-a") {
		t.Fatal("active participant claim was rejected")
	}
	if _, err := r.beginLeave(call, "phone-a", "leg-in", "leave-in"); err != nil {
		t.Fatalf("claimed inbound participant is not addressable by stable client id: %v", err)
	}
}

func TestInboundIdentityClaimCanArriveBeforeParticipantJoin(t *testing.T) {
	r := newCallSessionRegistry()
	call := sessionTestCall()
	if !r.claim(call.TraceID, "asterisk-participant", "phone-a") {
		t.Fatal("early participant claim was rejected")
	}
	r.joined(call, "", "asterisk-participant", "leg-in", "PJSIP/in", time.Now().UTC())

	if _, err := r.beginLeave(call, "phone-a", "leg-in", "leave-in"); err != nil {
		t.Fatalf("early identity claim was discarded: %v", err)
	}
}

func TestTerminalCallClearsPendingIdentityClaims(t *testing.T) {
	r := newCallSessionRegistry()
	call := sessionTestCall()
	if !r.claim(call.TraceID, "asterisk-participant", "phone-a") {
		t.Fatal("pre-terminal participant claim was rejected")
	}
	r.ended(call.Instance, call.EngineCallID, call.TraceID)
	if r.claim(call.TraceID, "late-participant", "phone-b") {
		t.Fatal("terminal call accepted a late participant claim")
	}
	r.joined(call, "", "asterisk-participant", "leg-in", "PJSIP/in", time.Now().UTC())

	if _, err := r.beginLeave(call, "phone-a", "leg-in", "leave-in"); !errors.Is(err, errCallParticipantNotFound) {
		t.Fatalf("terminal call retained a stale identity claim: %v", err)
	}
}

func TestTerminalCallReleasesItsIdempotencyOperations(t *testing.T) {
	r := newCallSessionRegistry()
	call := sessionTestCall()
	r.joined(call, "web-a", "web-a", "leg-a", "PJSIP/a", time.Now().UTC())
	if _, err := r.beginLeave(call, "web-a", "leg-a", "operation-1"); err != nil {
		t.Fatal(err)
	}
	r.ended(call.Instance, call.EngineCallID, call.TraceID)
	if _, found := r.operations["operation-1"]; found {
		t.Fatal("terminal call retained an idempotency operation")
	}
}

func TestParticipantControlAPISeparatesOutageFromExplicitLeave(t *testing.T) {
	application, err := New(Config{
		DataFile: filepath.Join(t.TempDir(), "state.json"), Telephony: TelephonyConfig{Type: "memory"},
		SIMAuth: SIMAuthConfig{Type: "disabled"}, Voice: VoiceConfig{EngineEventToken: "engine-secret"},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	gateway := &conferenceControlFixture{}
	application.gateway = gateway
	call := sessionTestCall()
	call.StartedAt, call.UpdatedAt = time.Now().UTC(), time.Now().UTC()
	if err := application.store.SaveCall(call); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(application.Handler())
	defer server.Close()

	participantEvent := func(event, client, connection, channel string, want int) {
		postEngineEvent(t, server.URL, "engine-secret", map[string]any{
			"instance": call.Instance, "event": event, "call_id": call.EngineCallID, "trace_id": call.TraceID,
			"args": []string{client, client, connection, channel},
		}, want)
	}
	participantEvent("participant_joined", "web-a", "leg-a", "PJSIP/a", http.StatusNoContent)
	participantEvent("participant_joined", "phone-b", "leg-b", "PJSIP/b", http.StatusNoContent)
	participantEvent("participant_left", "phone-b", "leg-b", "PJSIP/b", http.StatusNoContent)

	leave := func(client, connection, operation string) map[string]any {
		body, _ := json.Marshal(map[string]string{
			"trace_id": call.TraceID, "client_id": client, "connection_id": connection, "operation_id": operation,
		})
		response, err := http.Post(server.URL+"/api/v1/calls/participants/leave", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			payload, _ := io.ReadAll(response.Body)
			t.Fatalf("leave %s returned %d: %s", client, response.StatusCode, payload)
		}
		var decoded map[string]any
		if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
			t.Fatal(err)
		}
		return decoded
	}
	if result := leave("web-a", "leg-a", "leave-a"); result["carrier_ended"] != false {
		t.Fatalf("connected participant ended a call retained for disconnected peer: %#v", result)
	}
	if result := leave("web-a", "leg-a", "leave-a"); result["carrier_ended"] != false {
		t.Fatalf("idempotent retry changed result: %#v", result)
	}
	if result := leave("phone-b", "leg-b", "leave-b"); result["carrier_ended"] != true {
		t.Fatalf("last logical participant did not end carrier: %#v", result)
	}
	gateway.mu.Lock()
	defer gateway.mu.Unlock()
	if len(gateway.dropped) != 1 || gateway.dropped[0] != "PJSIP/a" {
		t.Fatalf("directed drops = %#v", gateway.dropped)
	}
	if len(gateway.conferences) != 1 || gateway.conferences[0] != call.EngineCallID {
		t.Fatalf("conference endings = %#v", gateway.conferences)
	}
}

func TestParticipantLeaveRetryRepeatsTheSameExternalAction(t *testing.T) {
	application, err := New(Config{
		DataFile: filepath.Join(t.TempDir(), "state.json"), Telephony: TelephonyConfig{Type: "memory"},
		SIMAuth: SIMAuthConfig{Type: "disabled"}, Voice: VoiceConfig{EngineEventToken: "engine-secret"},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	gateway := &conferenceControlFixture{dropErrors: []error{errors.New("temporary AMI failure")}}
	application.gateway = gateway
	call := sessionTestCall()
	call.StartedAt, call.UpdatedAt = time.Now().UTC(), time.Now().UTC()
	if err := application.store.SaveCall(call); err != nil {
		t.Fatal(err)
	}
	application.callSessions.joined(call, "web-a", "web-a", "leg-a", "PJSIP/a", time.Now().UTC())
	application.callSessions.joined(call, "phone-b", "phone-b", "leg-b", "PJSIP/b", time.Now().UTC())
	server := httptest.NewServer(application.Handler())
	defer server.Close()

	body, _ := json.Marshal(map[string]string{
		"trace_id": call.TraceID, "client_id": "web-a", "connection_id": "leg-a", "operation_id": "leave-retry",
	})
	post := func() int {
		response, err := http.Post(server.URL+"/api/v1/calls/participants/leave", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		return response.StatusCode
	}
	if got := post(); got != http.StatusBadGateway {
		t.Fatalf("first status = %d, want 502", got)
	}
	if got := post(); got != http.StatusOK {
		t.Fatalf("retry status = %d, want 200", got)
	}
	gateway.mu.Lock()
	defer gateway.mu.Unlock()
	if len(gateway.dropped) != 2 || gateway.dropped[0] != "PJSIP/a" || gateway.dropped[1] != "PJSIP/a" {
		t.Fatalf("retry changed directed action: %#v", gateway.dropped)
	}
}

func TestLateParticipantJoinAfterCallResultIsRejected(t *testing.T) {
	application, err := New(Config{
		DataFile: filepath.Join(t.TempDir(), "state.json"), Telephony: TelephonyConfig{Type: "memory"},
		SIMAuth: SIMAuthConfig{Type: "disabled"}, Voice: VoiceConfig{EngineEventToken: "engine-secret"},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	call := sessionTestCall()
	call.StartedAt, call.UpdatedAt = time.Now().UTC(), time.Now().UTC()
	endedAt := time.Now().UTC()
	call.EndedAt = &endedAt
	call.State = "ended"
	if err := application.store.SaveCall(call); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(application.Handler())
	defer server.Close()

	postEngineEvent(t, server.URL, "engine-secret", map[string]any{
		"instance": call.Instance, "event": "participant_joined", "call_id": call.EngineCallID, "trace_id": call.TraceID,
		"args": []string{"web-a", "web-a", "leg-a", "PJSIP/a"},
	}, http.StatusConflict)
	if _, err := application.callSessions.beginLeave(call, "web-a", "leg-a", "leave-a"); !errors.Is(err, errCallParticipantNotFound) {
		t.Fatalf("terminal call accepted late participant: %v", err)
	}
}
