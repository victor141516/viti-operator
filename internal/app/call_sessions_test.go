package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"maps"
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
	if got := r.joined(call, "phone-a", "phone-a", "leg-2", "PJSIP/web-2", now.Add(time.Second)); len(got.channelsToDrop) != 1 || got.channelsToDrop[0] != "PJSIP/web-1" {
		t.Fatalf("channels to drop = %#v, want old channel", got.channelsToDrop)
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

func TestSupersededDropRemainsPendingUntilAMIConfirmsIt(t *testing.T) {
	r := newCallSessionRegistry()
	call := sessionTestCall()
	now := time.Now().UTC()
	r.joined(call, "phone-a", "phone-a", "leg-1", "PJSIP/web-1", now)
	first := r.joined(call, "phone-a", "phone-a", "leg-2", "PJSIP/web-2", now.Add(time.Second))
	if len(first.channelsToDrop) != 1 || first.channelsToDrop[0] != "PJSIP/web-1" {
		t.Fatalf("first pending drops = %#v", first.channelsToDrop)
	}
	duplicate := r.joined(call, "phone-a", "phone-a", "leg-2", "PJSIP/web-2", now.Add(time.Second))
	if len(duplicate.channelsToDrop) != 1 || duplicate.channelsToDrop[0] != "PJSIP/web-1" {
		t.Fatalf("failed drop was not retained for retry: %#v", duplicate.channelsToDrop)
	}
	r.completeDrop(call, "PJSIP/web-1")
	completed := r.joined(call, "phone-a", "phone-a", "leg-2", "PJSIP/web-2", now.Add(time.Second))
	if len(completed.channelsToDrop) != 0 {
		t.Fatalf("confirmed drop remained pending: %#v", completed.channelsToDrop)
	}
}

func TestFinalExplicitLeaveClosesAdmissionBeforeExternalHangup(t *testing.T) {
	r := newCallSessionRegistry()
	call := sessionTestCall()
	now := time.Now().UTC()
	r.joined(call, "web-a", "web-a", "leg-a", "PJSIP/a", now)
	operation, err := r.beginLeave(call, "web-a", "leg-a", "leave-a")
	if err != nil || !operation.endCarrier {
		t.Fatalf("final leave = %+v, err=%v", operation, err)
	}

	late := r.joined(call, "phone-b", "phone-b", "leg-b", "PJSIP/b", now.Add(time.Second))
	if late.admitted || len(late.channelsToDrop) != 0 {
		t.Fatalf("join racing with terminal leave was admitted: %+v", late)
	}
	if _, err := r.beginLeave(call, "phone-b", "leg-b", "leave-b"); !errors.Is(err, errCallParticipantNotFound) {
		t.Fatalf("rejected racing join became a logical participant: %v", err)
	}
}

func TestLeaveDuringPendingReconnectMatchesPreviousGenerationOnly(t *testing.T) {
	r := newCallSessionRegistry()
	call := sessionTestCall()
	now := time.Now().UTC()
	r.joined(call, "phone-a", "phone-a", "leg-old", "PJSIP/old", now)
	r.left(call, "phone-a", "phone-a", "leg-old", now.Add(time.Second))

	operation, err := r.beginLeaveWithPrevious(call, "phone-a", "leg-pending", "leg-old", "leave-pending")
	if err != nil || operation.matchedConnectionID != "leg-old" || !operation.endCarrier {
		t.Fatalf("pending reconnect leave = %+v, err=%v", operation, err)
	}
	late := r.joined(call, "phone-a", "phone-a", "leg-pending", "PJSIP/pending", now.Add(2*time.Second))
	if late.admitted || len(late.channelsToDrop) != 0 {
		t.Fatalf("explicitly departed pending generation was resurrected: %+v", late)
	}
}

func TestPreviousGenerationCannotLeaveAnAlreadyRegisteredReplacement(t *testing.T) {
	r := newCallSessionRegistry()
	call := sessionTestCall()
	now := time.Now().UTC()
	r.joined(call, "phone-a", "phone-a", "leg-old", "PJSIP/old", now)
	r.joined(call, "phone-a", "phone-a", "leg-current", "PJSIP/current", now.Add(time.Second))

	if _, err := r.beginLeaveWithPrevious(call, "phone-a", "leg-pending", "leg-old", "stale-leave"); !errors.Is(err, errCallParticipantNotFound) {
		t.Fatalf("stale generations left the current replacement: %v", err)
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
	if late.admitted || len(late.channelsToDrop) != 0 {
		t.Fatalf("late departed generation was not rejected: %+v", late)
	}
	if _, err := r.beginLeave(call, "phone-a", "leg-1", "leave-again"); !errors.Is(err, errCallParticipantNotFound) {
		t.Fatalf("late join resurrected logical membership: %v", err)
	}
}

func TestAbortedAdmissionCannotBecomeGhostOwnershipOnLateDelivery(t *testing.T) {
	r := newCallSessionRegistry()
	call := sessionTestCall()
	now := time.Now().UTC()
	if joined := r.joined(call, "phone-a", "phone-a", "leg-attempt", "PJSIP/attempt", now); !joined.admitted {
		t.Fatalf("initial admission = %+v", joined)
	}
	r.abortJoin(call, "phone-a", "phone-a", "leg-attempt", "PJSIP/attempt", now.Add(time.Second))

	if got := len(r.sessions[callSessionKey(call.Instance, call.EngineCallID)].participants); got != 0 {
		t.Fatalf("aborted admission retained %d logical participants", got)
	}
	late := r.joined(call, "phone-a", "phone-a", "leg-attempt", "PJSIP/attempt", now)
	if late.admitted {
		t.Fatalf("late admission request crossed abort tombstone: %+v", late)
	}
	newer := r.joined(call, "phone-a", "phone-a", "leg-new", "PJSIP/new", now.Add(2*time.Second))
	if !newer.admitted {
		t.Fatalf("new generation did not supersede abort tombstone: %+v", newer)
	}
}

func TestAbortedReconnectRestoresPreviousLogicalMembership(t *testing.T) {
	r := newCallSessionRegistry()
	call := sessionTestCall()
	now := time.Now().UTC()
	r.joined(call, "phone-b", "phone-b", "leg-old", "PJSIP/old", now)
	r.left(call, "phone-b", "phone-b", "leg-old", now.Add(time.Second))
	r.joined(call, "phone-b", "phone-b", "leg-new", "PJSIP/new", now.Add(2*time.Second))
	r.abortJoin(call, "phone-b", "phone-b", "leg-new", "PJSIP/new", now.Add(3*time.Second))

	operation, err := r.beginLeaveWithPrevious(call, "phone-b", "leg-new", "leg-old", "leave-b")
	if err != nil || operation.matchedConnectionID != "leg-old" || !operation.endCarrier {
		t.Fatalf("previous membership was not restored: operation=%+v err=%v", operation, err)
	}
}

func TestSuccessiveAmbiguousReconnectsRestoreLastConfirmedGeneration(t *testing.T) {
	for _, abortOrder := range []struct {
		name        string
		connections []string
	}{
		{name: "oldest abort first", connections: []string{"leg-ambiguous-b", "leg-ambiguous-c"}},
		{name: "newest abort first", connections: []string{"leg-ambiguous-c", "leg-ambiguous-b"}},
	} {
		t.Run(abortOrder.name, func(t *testing.T) {
			r := newCallSessionRegistry()
			call := sessionTestCall()
			now := time.Now().UTC()
			r.joined(call, "phone-b", "phone-b", "leg-confirmed-a", "PJSIP/a", now)
			r.left(call, "phone-b", "phone-b", "leg-confirmed-a", now.Add(time.Second))
			r.joined(call, "phone-b", "phone-b", "leg-ambiguous-b", "PJSIP/b", now.Add(2*time.Second))
			r.joined(call, "phone-b", "phone-b", "leg-ambiguous-c", "PJSIP/c", now.Add(3*time.Second))

			channels := map[string]string{
				"leg-ambiguous-b": "PJSIP/b",
				"leg-ambiguous-c": "PJSIP/c",
			}
			for index, connectionID := range abortOrder.connections {
				r.abortJoin(
					call,
					"phone-b",
					"phone-b",
					connectionID,
					channels[connectionID],
					now.Add(time.Duration(4+index)*time.Second),
				)
			}

			operation, err := r.beginLeaveWithPrevious(
				call,
				"phone-b",
				"leg-pending-d",
				"leg-confirmed-a",
				"leave-after-ambiguous-chain",
			)
			if err != nil || operation.matchedConnectionID != "leg-confirmed-a" || !operation.endCarrier {
				t.Fatalf("last confirmed membership was not restored: operation=%+v err=%v", operation, err)
			}
		})
	}
}

func TestAbortBeforeExplicitHangupStillHonorsLastParticipantIntent(t *testing.T) {
	r := newCallSessionRegistry()
	call := sessionTestCall()
	now := time.Now().UTC()
	r.joined(call, "phone-a", "phone-a", "leg-attempt", "PJSIP/attempt", now)
	r.abortJoin(call, "phone-a", "phone-a", "leg-attempt", "PJSIP/attempt", now.Add(time.Second))

	operation, err := r.beginLeave(call, "phone-a", "leg-attempt", "leave-after-abort")
	if err != nil || operation.matchedConnectionID != "leg-attempt" || !operation.endCarrier {
		t.Fatalf("explicit hangup could not consume aborted generation: operation=%+v err=%v", operation, err)
	}
}

func TestRejectedConnectionCollisionAbortCannotRemoveValidChannel(t *testing.T) {
	r := newCallSessionRegistry()
	call := sessionTestCall()
	now := time.Now().UTC()
	r.joined(call, "phone-a", "phone-a", "leg-shared", "PJSIP/valid", now)
	collision := r.joined(call, "phone-a", "phone-a", "leg-shared", "PJSIP/rejected", now.Add(time.Second))
	if collision.admitted {
		t.Fatalf("connection collision was admitted: %+v", collision)
	}
	r.abortJoin(call, "phone-a", "phone-a", "leg-shared", "PJSIP/rejected", now.Add(2*time.Second))

	operation, err := r.beginLeave(call, "phone-a", "leg-shared", "leave-valid")
	if err != nil || operation.channel != "PJSIP/valid" {
		t.Fatalf("collision abort revoked valid channel: operation=%+v err=%v", operation, err)
	}
}

func TestDelayedAbortCannotRemoveANewerAdmittedGeneration(t *testing.T) {
	r := newCallSessionRegistry()
	call := sessionTestCall()
	now := time.Now().UTC()
	r.joined(call, "phone-a", "phone-a", "leg-old", "PJSIP/old", now)
	r.joined(call, "phone-a", "phone-a", "leg-new", "PJSIP/new", now.Add(2*time.Second))
	r.abortJoin(call, "phone-a", "phone-a", "leg-old", "PJSIP/old", now.Add(time.Second))

	operation, err := r.beginLeave(call, "phone-a", "leg-new", "leave-new")
	if err != nil || operation.matchedConnectionID != "leg-new" {
		t.Fatalf("delayed old abort removed replacement: operation=%+v err=%v", operation, err)
	}
}

func TestLateAbortEndsCarrierAfterAllRealParticipantsExplicitlyLeft(t *testing.T) {
	r := newCallSessionRegistry()
	call := sessionTestCall()
	now := time.Now().UTC()
	r.joined(call, "web-a", "web-a", "leg-a", "PJSIP/a", now)
	r.joined(call, "phone-b", "phone-b", "leg-ambiguous", "PJSIP/b", now)
	operation, err := r.beginLeave(call, "web-a", "leg-a", "leave-a")
	if err != nil || operation.endCarrier {
		t.Fatalf("first explicit leave = %+v, err=%v", operation, err)
	}

	if !r.abortJoin(call, "phone-b", "phone-b", "leg-ambiguous", "PJSIP/b", now.Add(time.Second)) {
		t.Fatal("late abort did not request carrier termination after the real participant left")
	}
	if !r.abortJoin(call, "phone-b", "phone-b", "leg-ambiguous", "PJSIP/b", now.Add(time.Second)) {
		t.Fatal("replayed abort did not preserve the same carrier termination decision")
	}
	late := r.joined(call, "web-c", "web-c", "leg-c", "PJSIP/c", now.Add(2*time.Second))
	if late.admitted {
		t.Fatalf("join entered while abort-driven carrier termination was pending: %+v", late)
	}
}

func TestClaimMigratesAbortTombstoneBeforeDelayedIncomingJoin(t *testing.T) {
	r := newCallSessionRegistry()
	call := sessionTestCall()
	now := time.Now().UTC()
	r.abortJoin(call, "", "asterisk-participant", "leg-in", "PJSIP/in", now.Add(time.Second))
	if !r.claim(call.TraceID, "asterisk-participant", "phone-a") {
		t.Fatal("claim was rejected")
	}

	late := r.joined(call, "", "asterisk-participant", "leg-in", "PJSIP/in", now)
	if late.admitted {
		t.Fatalf("claim changed the key and bypassed abort tombstone: %+v", late)
	}
	newer := r.joined(call, "", "asterisk-participant", "leg-new", "PJSIP/new", now.Add(2*time.Second))
	if !newer.admitted {
		t.Fatalf("new incoming generation could not pass migrated tombstone: %+v", newer)
	}
}

func TestNewGenerationCanJoinAfterExplicitDeparture(t *testing.T) {
	r := newCallSessionRegistry()
	call := sessionTestCall()
	now := time.Now().UTC().Add(-time.Minute)
	r.joined(call, "phone-a", "phone-a", "leg-1", "PJSIP/web-1", now)
	r.joined(call, "web-b", "web-b", "leg-b", "PJSIP/web-b", now)
	if _, err := r.beginLeave(call, "phone-a", "leg-1", "leave-1"); err != nil {
		t.Fatal(err)
	}

	joined := r.joined(call, "phone-a", "phone-a", "leg-2", "PJSIP/web-2", time.Now().UTC().Add(time.Second))
	if len(joined.channelsToDrop) != 0 {
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

func TestParticipantJoinRetriesASupersededDropAfterAMIFailure(t *testing.T) {
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
	now := time.Now().UTC()
	application.callSessions.joined(call, "web-a", "web-a", "leg-old", "PJSIP/old", now)
	server := httptest.NewServer(application.Handler())
	defer server.Close()

	payload, _ := json.Marshal(map[string]any{
		"instance": call.Instance, "event": "participant_joined", "call_id": call.EngineCallID, "trace_id": call.TraceID,
		"recorded_at_ns": now.Add(time.Second).UnixNano(),
		"args":           []string{"web-a", "web-a", "leg-new", "PJSIP/new"},
	})
	post := func() int {
		request, _ := http.NewRequest(http.MethodPost, server.URL+"/api/engine/event", bytes.NewReader(payload))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Viti-Engine-Token", "engine-secret")
		response, requestErr := http.DefaultClient.Do(request)
		if requestErr != nil {
			t.Fatal(requestErr)
		}
		defer response.Body.Close()
		return response.StatusCode
	}
	if got := post(); got != http.StatusBadGateway {
		t.Fatalf("first status = %d, want 502", got)
	}
	if got := post(); got != http.StatusNoContent {
		t.Fatalf("retry status = %d, want 204", got)
	}
	operation, err := application.callSessions.beginLeave(call, "web-a", "leg-new", "leave-new")
	if err != nil || operation.channel != "PJSIP/new" {
		t.Fatalf("replacement was not retained after drop retry: %+v, err=%v", operation, err)
	}
	gateway.mu.Lock()
	defer gateway.mu.Unlock()
	if len(gateway.dropped) != 2 || gateway.dropped[0] != "PJSIP/old" || gateway.dropped[1] != "PJSIP/old" {
		t.Fatalf("superseded drop attempts = %#v", gateway.dropped)
	}
}

func TestFailedFinalLeaveRejectsALaterJoinBeforeRetry(t *testing.T) {
	application, err := New(Config{
		DataFile: filepath.Join(t.TempDir(), "state.json"), Telephony: TelephonyConfig{Type: "memory"},
		SIMAuth: SIMAuthConfig{Type: "disabled"}, Voice: VoiceConfig{EngineEventToken: "engine-secret"},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	gateway := &conferenceControlFixture{endErrors: []error{errors.New("temporary AMI failure")}}
	application.gateway = gateway
	call := sessionTestCall()
	call.StartedAt, call.UpdatedAt = time.Now().UTC(), time.Now().UTC()
	if err := application.store.SaveCall(call); err != nil {
		t.Fatal(err)
	}
	application.callSessions.joined(call, "web-a", "web-a", "leg-a", "PJSIP/a", time.Now().UTC())
	server := httptest.NewServer(application.Handler())
	defer server.Close()

	leaveBody, _ := json.Marshal(map[string]string{
		"trace_id": call.TraceID, "client_id": "web-a", "connection_id": "leg-a", "operation_id": "leave-a",
	})
	leave := func() int {
		response, requestErr := http.Post(server.URL+"/api/v1/calls/participants/leave", "application/json", bytes.NewReader(leaveBody))
		if requestErr != nil {
			t.Fatal(requestErr)
		}
		defer response.Body.Close()
		return response.StatusCode
	}
	if got := leave(); got != http.StatusBadGateway {
		t.Fatalf("first final leave status = %d, want 502", got)
	}
	postEngineEvent(t, server.URL, "engine-secret", map[string]any{
		"instance": call.Instance, "event": "participant_joined", "call_id": call.EngineCallID, "trace_id": call.TraceID,
		"args": []string{"phone-b", "phone-b", "leg-b", "PJSIP/b"},
	}, http.StatusGone)
	if got := leave(); got != http.StatusOK {
		t.Fatalf("final leave retry status = %d, want 200", got)
	}
	gateway.mu.Lock()
	defer gateway.mu.Unlock()
	if len(gateway.dropped) != 0 {
		t.Fatalf("synchronously rejected join should hang up its own channel: %#v", gateway.dropped)
	}
	if len(gateway.conferences) != 2 {
		t.Fatalf("final conference attempts = %#v", gateway.conferences)
	}
}

func TestAbortedParticipantEventRevokesAnAmbiguouslyAdmittedJoin(t *testing.T) {
	application, err := New(Config{
		DataFile: filepath.Join(t.TempDir(), "state.json"), Telephony: TelephonyConfig{Type: "memory"},
		SIMAuth: SIMAuthConfig{Type: "disabled"}, Voice: VoiceConfig{EngineEventToken: "engine-secret"},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	call := sessionTestCall()
	call.StartedAt, call.UpdatedAt = time.Now().UTC(), time.Now().UTC()
	if err := application.store.SaveCall(call); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(application.Handler())
	defer server.Close()
	now := time.Now().UTC()
	base := map[string]any{
		"instance": call.Instance, "call_id": call.EngineCallID, "trace_id": call.TraceID,
		"args": []string{"web-a", "web-a", "leg-a", "PJSIP/a"},
	}
	joined := maps.Clone(base)
	joined["event"] = "participant_joined"
	joined["recorded_at_ns"] = now.UnixNano()
	postEngineEvent(t, server.URL, "engine-secret", joined, http.StatusNoContent)
	aborted := maps.Clone(base)
	aborted["event"] = "participant_join_aborted"
	aborted["recorded_at_ns"] = now.Add(time.Second).UnixNano()
	postEngineEvent(t, server.URL, "engine-secret", aborted, http.StatusNoContent)

	if got := len(application.callSessions.sessions[callSessionKey(call.Instance, call.EngineCallID)].participants); got != 0 {
		t.Fatalf("aborted admission retained %d logical participants", got)
	}
}

func TestAbortDrivenCarrierTerminationRetriesAfterAMIFailure(t *testing.T) {
	application, err := New(Config{
		DataFile: filepath.Join(t.TempDir(), "state.json"), Telephony: TelephonyConfig{Type: "memory"},
		SIMAuth: SIMAuthConfig{Type: "disabled"}, Voice: VoiceConfig{EngineEventToken: "engine-secret"},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	gateway := &conferenceControlFixture{endErrors: []error{errors.New("temporary AMI failure")}}
	application.gateway = gateway
	call := sessionTestCall()
	call.StartedAt, call.UpdatedAt = time.Now().UTC(), time.Now().UTC()
	if err := application.store.SaveCall(call); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	application.callSessions.joined(call, "web-a", "web-a", "leg-a", "PJSIP/a", now)
	application.callSessions.joined(call, "phone-b", "phone-b", "leg-b", "PJSIP/b", now)
	leave, err := application.callSessions.beginLeave(call, "web-a", "leg-a", "leave-a")
	if err != nil || leave.endCarrier {
		t.Fatalf("first leave = %+v, err=%v", leave, err)
	}
	application.callSessions.completeLeave(leave.operationID)
	server := httptest.NewServer(application.Handler())
	defer server.Close()
	payload := map[string]any{
		"instance": call.Instance, "event": "participant_join_aborted", "call_id": call.EngineCallID, "trace_id": call.TraceID,
		"recorded_at_ns": now.Add(time.Second).UnixNano(),
		"args":           []string{"phone-b", "phone-b", "leg-b", "PJSIP/b"},
	}

	postEngineEvent(t, server.URL, "engine-secret", payload, http.StatusBadGateway)
	postEngineEvent(t, server.URL, "engine-secret", payload, http.StatusNoContent)
	gateway.mu.Lock()
	defer gateway.mu.Unlock()
	if len(gateway.conferences) != 2 || gateway.conferences[0] != call.EngineCallID || gateway.conferences[1] != call.EngineCallID {
		t.Fatalf("abort-driven conference retries = %#v", gateway.conferences)
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
	}, http.StatusGone)
	postEngineEvent(t, server.URL, "engine-secret", map[string]any{
		"instance": call.Instance, "event": "participant_left", "call_id": call.EngineCallID, "trace_id": call.TraceID,
		"args": []string{"web-a", "web-a", "leg-a", "PJSIP/a"},
	}, http.StatusNoContent)
	if _, err := application.callSessions.beginLeave(call, "web-a", "leg-a", "leave-a"); !errors.Is(err, errCallParticipantNotFound) {
		t.Fatalf("terminal call accepted late participant: %v", err)
	}
}
