package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type readinessFixture struct {
	ready bool
}

func (fixture *readinessFixture) AuthenticateAKA(context.Context, []byte, []byte) (AKAResult, error) {
	return AKAResult{}, errors.New("not used")
}

func (fixture *readinessFixture) CheckSIMReady(context.Context) error {
	if !fixture.ready {
		return errors.New("OpenStick disconnected for test")
	}
	return nil
}

func TestEngineIncomingSMSIsPersistedAndIdempotent(t *testing.T) {
	cfg := Config{
		DataFile: filepath.Join(t.TempDir(), "state.json"), Telephony: TelephonyConfig{Type: "memory"},
		SIMAuth: SIMAuthConfig{Type: "disabled"}, Voice: VoiceConfig{EngineEventToken: "test-token"},
	}
	app, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(app.Handler())
	defer server.Close()
	recordedAtNS := time.Date(2026, 9, 8, 1, 2, 3, 456, time.UTC).UnixNano()
	payload := map[string]any{
		"instance": "engine-1", "event": "sms_in", "recorded_at_ns": recordedAtNS,
		"args": []string{"BANK", base64.StdEncoding.EncodeToString([]byte("Your test code is 123456"))},
	}
	postEngineEvent(t, server.URL, "test-token", payload, http.StatusOK)
	postEngineEvent(t, server.URL, "test-token", payload, http.StatusOK)

	messages := app.store.Messages()
	if len(messages) != 1 {
		t.Fatalf("got %d messages, want one idempotent incoming message", len(messages))
	}
	message := messages[0]
	if message.Direction != DirectionIncoming || message.From != "BANK" || message.Text != "Your test code is 123456" || message.State != "received" {
		t.Fatalf("unexpected incoming message: %#v", message)
	}
	if message.CreatedAt.UnixNano() != recordedAtNS {
		t.Fatalf("created_at=%s, want engine timestamp %d", message.CreatedAt, recordedAtNS)
	}

	payload["args"] = []string{"BANK", "not base64!"}
	postEngineEvent(t, server.URL, "test-token", payload, http.StatusBadRequest)
}

func TestEngineSMSProtocolEventsUpdateOriginalMessage(t *testing.T) {
	cfg := Config{
		DataFile: filepath.Join(t.TempDir(), "state.json"), Telephony: TelephonyConfig{Type: "memory"},
		SIMAuth: SIMAuthConfig{Type: "disabled"}, Voice: VoiceConfig{EngineEventToken: "test-token"},
	}
	application, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	message := Message{
		ID: "sms-engine", Direction: DirectionOutgoing, To: "+34600000000", Text: "hello",
		State: "sending", DeliveryState: "pending", CreatedAt: now, UpdatedAt: now,
	}
	if err := application.store.SaveMessage(message); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(application.Handler())
	defer server.Close()

	statusReport := map[string]any{
		"instance": "engine-1", "event": "sms_protocol",
		"args": []string{"status_report", "sms-engine", "+34600000000", "1", "1", "7", "0", "0", "-1"},
	}
	// A fast report is retained by the engine outbox when its persisted reference
	// mapping has not arrived yet.
	postEngineEvent(t, server.URL, "test-token", statusReport, http.StatusConflict)
	postEngineEvent(t, server.URL, "test-token", map[string]any{
		"instance": "engine-1", "event": "sms_protocol",
		"args": []string{"submitted", "sms-engine", "+34600000000", "1", "1", "7", "0", "-1", "-1"},
	}, http.StatusOK)
	postEngineEvent(t, server.URL, "test-token", statusReport, http.StatusOK)
	postEngineEvent(t, server.URL, "test-token", statusReport, http.StatusOK)

	stored, ok := application.store.GetMessage("sms-engine")
	if !ok || stored.State != "delivered" || stored.DeliveryState != "delivered" || len(stored.Parts) != 1 {
		t.Fatalf("protocol lifecycle was not coalesced: %+v", stored)
	}
	if stored.Parts[0].TPReference == nil || *stored.Parts[0].TPReference != 0 {
		t.Fatalf("zero TP-MR was lost: %+v", stored.Parts[0])
	}
	if stored.StatusDetail == "" || stored.UpdatedAt.Before(now) {
		t.Fatalf("status explanation/timestamp missing: %+v", stored)
	}
}

func TestEngineSMSProtocolEventValidation(t *testing.T) {
	cfg := Config{
		DataFile: filepath.Join(t.TempDir(), "state.json"), Telephony: TelephonyConfig{Type: "memory"},
		SIMAuth: SIMAuthConfig{Type: "disabled"}, Voice: VoiceConfig{EngineEventToken: "test-token"},
	}
	application, err := New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(application.Handler())
	defer server.Close()
	postEngineEvent(t, server.URL, "test-token", map[string]any{
		"event": "sms_protocol", "args": []string{"rp_error"},
	}, http.StatusBadRequest)
}

func TestRuntimeStatusTracksOpenStickAndTunnelTransitions(t *testing.T) {
	cfg := Config{
		DataFile: filepath.Join(t.TempDir(), "state.json"), Telephony: TelephonyConfig{Type: "memory"},
		SIMAuth: SIMAuthConfig{Type: "disabled"}, Voice: VoiceConfig{EngineEventToken: "test-token"},
	}
	app, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	readiness := &readinessFixture{}
	app.auth = readiness
	server := httptest.NewServer(app.Handler())
	defer server.Close()

	status := runtimeStatusForTest(t, server.URL)
	if !status.Ready || status.SIMReady || !status.TunnelReady || !status.IMSReady {
		t.Fatalf("usable carrier path must remain ready without SIM authentication: %#v", status)
	}
	readiness.ready = true
	assertRuntimeReady(t, server.URL, true)
	postTunnelEvent(t, server.URL, "process-one", 1, true, time.Now().UTC(), http.StatusNoContent)
	started := time.Now().UTC()
	if err := app.store.SaveCall(Call{ID: "active-before-disconnect", Direction: DirectionOutgoing, Peer: "+15555550101", State: "active", StartedAt: started, UpdatedAt: started}); err != nil {
		t.Fatal(err)
	}

	readiness.ready = false
	status = runtimeStatusForTest(t, server.URL)
	if !status.Ready || status.SIMReady || !status.TunnelReady || !status.IMSReady {
		t.Fatalf("SIM loss must preserve independent session evidence: %#v", status)
	}
	stillActive, _ := app.store.GetCall("active-before-disconnect")
	if stillActive.State != "active" || stillActive.EndedAt != nil {
		t.Fatalf("SIM loss proactively ended the call: %#v", stillActive)
	}
	readiness.ready = true
	assertRuntimeReady(t, server.URL, true)
	stillActive, _ = app.store.GetCall("active-before-disconnect")
	if stillActive.State != "active" || stillActive.EndedAt != nil {
		t.Fatalf("SIM reconnection disrupted the call: %#v", stillActive)
	}

	postTunnelEvent(t, server.URL, "process-one", 2, false, time.Now().UTC(), http.StatusNoContent)
	assertRuntimeReady(t, server.URL, false)
	interrupted, ok := app.store.GetCall("active-before-disconnect")
	if !ok || interrupted.State != "interrupted" || interrupted.EndedAt == nil {
		t.Fatalf("open call was not released on tunnel loss: %#v", interrupted)
	}
	payload, _ := json.Marshal(map[string]string{"to": "+15555550101", "text": "must not queue while disconnected"})
	response, err := http.Post(server.URL+"/api/v1/messages", "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable || len(app.store.Messages()) != 0 {
		t.Fatalf("disconnected send status=%d messages=%d, want 503 and no queued message", response.StatusCode, len(app.store.Messages()))
	}
	postTunnelEvent(t, server.URL, "process-one", 3, true, time.Now().UTC(), http.StatusNoContent)
	assertRuntimeReady(t, server.URL, true)
}

func TestTunnelTransitionsAreOrderedIdempotentAndDurableAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	newApp := func() *App {
		app, err := New(Config{
			DataFile: path, Telephony: TelephonyConfig{Type: "memory"},
			SIMAuth: SIMAuthConfig{Type: "disabled"}, Voice: VoiceConfig{EngineEventToken: "test-token"},
		}, slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err != nil {
			t.Fatal(err)
		}
		return app
	}
	app := newApp()
	server := httptest.NewServer(app.Handler())
	now := time.Now().UTC()
	postTunnelEvent(t, server.URL, "process-one", 1, true, now, http.StatusNoContent)
	if err := app.store.SaveCall(Call{ID: "call-one", Direction: DirectionOutgoing, Peer: "+15555550101", State: "active", StartedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	postTunnelEvent(t, server.URL, "process-one", 2, false, now.Add(2*time.Second), http.StatusNoContent)
	first, _ := app.store.GetCall("call-one")
	if first.EndedAt == nil {
		t.Fatal("accepted up-to-down transition did not interrupt the open call")
	}
	oldCallTime := now.Add(-10 * time.Minute)
	if err := app.store.SaveCall(Call{ID: "call-two", Direction: DirectionOutgoing, Peer: "+15555550102", State: "active", StartedAt: oldCallTime, UpdatedAt: oldCallTime}); err != nil {
		t.Fatal(err)
	}
	postTunnelEvent(t, server.URL, "process-one", 2, false, now.Add(2*time.Second), http.StatusNoContent)
	postTunnelEvent(t, server.URL, "process-one", 1, true, now, http.StatusNoContent)
	postTunnelEvent(t, server.URL, "process-two", 1, true, now.Add(3*time.Second), http.StatusNoContent)
	postTunnelEvent(t, server.URL, "process-one", 3, false, now.Add(4*time.Second), http.StatusNoContent)
	postTunnelEvent(t, server.URL, "never-established", 1, false, now.Add(5*time.Second), http.StatusNoContent)
	second, _ := app.store.GetCall("call-two")
	if second.EndedAt != nil {
		t.Fatalf("duplicate/delayed/unowned transition interrupted a recovered call: %#v", second)
	}
	server.Close()

	restarted := newApp()
	if !restarted.tunnel.Seen || !restarted.tunnel.Ready || restarted.tunnel.Generation != "process-two" || restarted.tunnel.Sequence != 1 {
		t.Fatalf("manager restart did not restore the durable tunnel checkpoint: %#v", restarted.tunnel)
	}
	restartedServer := httptest.NewServer(restarted.Handler())
	defer restartedServer.Close()
	second, _ = restarted.store.GetCall("call-two")
	if second.EndedAt != nil || second.State != "active" {
		t.Fatalf("manager restart closed a long carrier call using age alone: %#v", second)
	}
	postTunnelEvent(t, restartedServer.URL, "process-one", 4, false, now.Add(5*time.Second), http.StatusNoContent)
	second, _ = restarted.store.GetCall("call-two")
	if second.EndedAt != nil {
		t.Fatalf("retired transition was revived after manager restart: %#v", second)
	}
	postEngineEvent(t, restartedServer.URL, "test-token", map[string]any{
		"instance": "engine-1", "event": "tunnel_start", "args": []string{"replacement process"},
		"recorded_at_ns": now.Add(6 * time.Second).UnixNano(), "tunnel_generation": "process-three", "tunnel_sequence": 1,
	}, http.StatusNoContent)
	second, _ = restarted.store.GetCall("call-two")
	if second.EndedAt == nil || second.State != "interrupted" || restarted.tunnel.Ready {
		t.Fatalf("durable replacement-process start did not retire the dead data plane: call=%#v tunnel=%#v", second, restarted.tunnel)
	}
}

func TestLegacyTunnelEventsCanTakeOwnershipAgainAfterModernEngineRollback(t *testing.T) {
	app, err := New(Config{
		DataFile: filepath.Join(t.TempDir(), "state.json"), Telephony: TelephonyConfig{Type: "memory"},
		SIMAuth: SIMAuthConfig{Type: "disabled"}, Voice: VoiceConfig{EngineEventToken: "test-token"},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(app.Handler())
	defer server.Close()
	now := time.Now().UTC()
	legacy := func(event string, at time.Time) {
		postEngineEvent(t, server.URL, "test-token", map[string]any{
			"instance": "engine-1", "event": event, "recorded_at_ns": at.UnixNano(),
		}, http.StatusNoContent)
	}
	legacy("tunnel_up", now)
	postTunnelEvent(t, server.URL, "modern", 1, true, now.Add(2*time.Second), http.StatusNoContent)
	legacy("tunnel_up", now.Add(time.Second))
	checkpoint, ok := app.store.EngineTunnelCheckpoint("engine-1")
	if !ok || checkpoint.Generation != "modern" || !checkpoint.Ready {
		t.Fatalf("delayed legacy up retired a newer modern generation: %#v", checkpoint)
	}
	postTunnelEvent(t, server.URL, "modern", 2, false, now.Add(3*time.Second), http.StatusNoContent)
	legacy("tunnel_up", now.Add(4*time.Second))
	checkpoint, ok = app.store.EngineTunnelCheckpoint("engine-1")
	if !ok || !checkpoint.Ready || !strings.HasPrefix(checkpoint.Generation, "legacy:") {
		t.Fatalf("legacy rollback did not reclaim tunnel ownership: %#v", checkpoint)
	}
	if err := app.store.SaveCall(Call{ID: "legacy-call", Direction: DirectionOutgoing, Peer: "+15555550103", State: "active", StartedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	legacy("tunnel_down", now.Add(5*time.Second))
	call, _ := app.store.GetCall("legacy-call")
	if call.EndedAt == nil || call.State != "interrupted" {
		t.Fatalf("legacy down event was not correlated to its rollback era: %#v", call)
	}
}

func TestDelayedUnobservedModernGenerationCannotRetireNewerOwner(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "state.json")
	app, err := New(Config{
		DataFile: path, Telephony: TelephonyConfig{Type: "memory"},
		SIMAuth: SIMAuthConfig{Type: "disabled"}, Voice: VoiceConfig{EngineEventToken: "test-token"},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(app.Handler())
	defer server.Close()
	baseAt := time.Date(2026, 9, 10, 9, 0, 0, 0, time.UTC)
	oldStartAt := baseAt.Add(3 * time.Second)
	newStartAt := baseAt.Add(4 * time.Second)
	newUpAt := baseAt.Add(5 * time.Second)
	oldStart := map[string]any{
		"instance": "engine-1", "event": "tunnel_start", "recorded_at_ns": oldStartAt.UnixNano(),
		"tunnel_generation": "process-c", "tunnel_sequence": 1, "args": []string{"old process"},
	}

	originalPath := app.store.path
	app.store.path = directory // Atomic replacement over this directory fails.
	postEngineEvent(t, server.URL, "test-token", oldStart, http.StatusInternalServerError)
	app.store.path = originalPath
	postEngineEvent(t, server.URL, "test-token", map[string]any{
		"instance": "engine-1", "event": "tunnel_start", "recorded_at_ns": newStartAt.UnixNano(),
		"tunnel_generation": "process-d", "tunnel_sequence": 1, "args": []string{"new process"},
	}, http.StatusNoContent)
	postTunnelEvent(t, server.URL, "process-d", 2, true, newUpAt, http.StatusNoContent)

	// The outbox retries C after D has already become the live owner.
	postEngineEvent(t, server.URL, "test-token", oldStart, http.StatusNoContent)
	checkpoint, ok := app.store.EngineTunnelCheckpoint("engine-1")
	if !ok || checkpoint.Generation != "process-d" || !checkpoint.Ready || checkpoint.Sequence != 2 {
		t.Fatalf("delayed older generation retired the current owner: %#v", checkpoint)
	}
}

func TestTunnelTransitionPersistenceFailureRollsBackCallAndCursor(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "state.json")
	app, err := New(Config{
		DataFile: path, Telephony: TelephonyConfig{Type: "memory"},
		SIMAuth: SIMAuthConfig{Type: "disabled"}, Voice: VoiceConfig{EngineEventToken: "test-token"},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(app.Handler())
	defer server.Close()
	now := time.Now().UTC()
	postTunnelEvent(t, server.URL, "process-one", 1, true, now, http.StatusNoContent)
	if err := app.store.SaveCall(Call{ID: "active-call", Direction: DirectionOutgoing, Peer: "+15555550101", State: "active", StartedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	app.store.path = directory // Renaming a regular file over this directory must fail.
	postTunnelEvent(t, server.URL, "process-one", 2, false, now.Add(time.Second), http.StatusInternalServerError)
	call, _ := app.store.GetCall("active-call")
	if call.EndedAt != nil || call.State != "active" {
		t.Fatalf("failed persistence leaked a partial call transition: %#v", call)
	}
	app.store.path = path
	postTunnelEvent(t, server.URL, "process-one", 2, false, now.Add(time.Second), http.StatusNoContent)
	call, _ = app.store.GetCall("active-call")
	if call.EndedAt == nil || call.State != "interrupted" {
		t.Fatalf("rolled-back cursor did not accept the durable retry: %#v", call)
	}
}

func TestHistoricalTunnelTransitionDoesNotInterruptALaterCall(t *testing.T) {
	app, err := New(Config{
		DataFile: filepath.Join(t.TempDir(), "state.json"), Telephony: TelephonyConfig{Type: "memory"},
		SIMAuth: SIMAuthConfig{Type: "disabled"}, Voice: VoiceConfig{EngineEventToken: "test-token"},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(app.Handler())
	defer server.Close()
	now := time.Now().UTC()
	postTunnelEvent(t, server.URL, "process-one", 1, true, now, http.StatusNoContent)
	started := now.Add(3 * time.Second)
	if err := app.store.SaveCall(Call{ID: "later-call", Direction: DirectionOutgoing, Peer: "+15555550104", State: "active", StartedAt: started, UpdatedAt: started}); err != nil {
		t.Fatal(err)
	}
	postTunnelEvent(t, server.URL, "process-one", 2, false, now.Add(time.Second), http.StatusNoContent)
	call, _ := app.store.GetCall("later-call")
	if call.EndedAt != nil || call.State != "active" {
		t.Fatalf("historical transport event interrupted a later call: %#v", call)
	}
}

func TestDelayedCallStartUsesEngineTimeBeforeTunnelFailure(t *testing.T) {
	app, err := New(Config{
		DataFile: filepath.Join(t.TempDir(), "state.json"), Telephony: TelephonyConfig{Type: "memory"},
		SIMAuth: SIMAuthConfig{Type: "disabled"}, Voice: VoiceConfig{EngineEventToken: "test-token"},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(app.Handler())
	defer server.Close()

	tunnelUpAt := time.Date(2026, 9, 10, 10, 0, 0, 0, time.UTC)
	callStartedAt := tunnelUpAt.Add(time.Second)
	tunnelDownAt := tunnelUpAt.Add(2 * time.Second)
	postTunnelEvent(t, server.URL, "process-one", 1, true, tunnelUpAt, http.StatusNoContent)

	// The manager receives this start after the engine produced both it and the
	// tunnel failure. Its recorded time, not HTTP arrival time, determines which
	// side of the transport failure the call belongs to.
	postEngineEvent(t, server.URL, "test-token", map[string]any{
		"instance": "engine-1", "event": "call_out", "call_id": "delayed-call",
		"recorded_at_ns": callStartedAt.UnixNano(), "args": []string{"+15555550105"},
	}, http.StatusOK)
	postTunnelEvent(t, server.URL, "process-one", 2, false, tunnelDownAt, http.StatusNoContent)

	call, ok := app.store.GetCallByEngineID("engine-1", "delayed-call")
	if !ok {
		t.Fatal("delayed call event did not create a call")
	}
	if !call.StartedAt.Equal(callStartedAt) {
		t.Fatalf("started at %v, want engine event time %v", call.StartedAt, callStartedAt)
	}
	if call.State != "interrupted" || call.EndedAt == nil || !call.EndedAt.Equal(tunnelDownAt) {
		t.Fatalf("transport failure did not interrupt the pre-existing call: %#v", call)
	}
}

func TestDelayedCallStartAfterActiveStillPrecedesTunnelFailure(t *testing.T) {
	app, err := New(Config{
		DataFile: filepath.Join(t.TempDir(), "state.json"), Telephony: TelephonyConfig{Type: "memory"},
		SIMAuth: SIMAuthConfig{Type: "disabled"}, Voice: VoiceConfig{EngineEventToken: "test-token"},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(app.Handler())
	defer server.Close()

	tunnelUpAt := time.Date(2026, 9, 10, 10, 30, 0, 0, time.UTC)
	callStartedAt := tunnelUpAt.Add(time.Second)
	tunnelDownAt := tunnelUpAt.Add(1200 * time.Millisecond)
	callActiveAt := tunnelUpAt.Add(1400 * time.Millisecond)
	postTunnelEvent(t, server.URL, "process-one", 1, true, tunnelUpAt, http.StatusNoContent)

	// The active hook overtakes the earlier start hook on delivery.
	postEngineEvent(t, server.URL, "test-token", map[string]any{
		"instance": "engine-1", "event": "call_active", "call_id": "reordered-call",
		"recorded_at_ns": callActiveAt.UnixNano(), "args": []string{"out", "+15555550107"},
	}, http.StatusOK)
	postEngineEvent(t, server.URL, "test-token", map[string]any{
		"instance": "engine-1", "event": "call_out", "call_id": "reordered-call",
		"recorded_at_ns": callStartedAt.UnixNano(), "args": []string{"+15555550107"},
	}, http.StatusOK)

	beforeDown, _ := app.store.GetCallByEngineID("engine-1", "reordered-call")
	if beforeDown.State != "active" || !beforeDown.StartedAt.Equal(callStartedAt) || !beforeDown.UpdatedAt.Equal(callActiveAt) {
		t.Fatalf("delayed start regressed the active lifecycle: %#v", beforeDown)
	}
	postTunnelEvent(t, server.URL, "process-one", 2, false, tunnelDownAt, http.StatusNoContent)

	call, _ := app.store.GetCallByEngineID("engine-1", "reordered-call")
	if call.State != "interrupted" || call.EndedAt == nil || !call.EndedAt.Equal(tunnelDownAt) {
		t.Fatalf("transport failure did not interrupt reordered call: %#v", call)
	}
	if call.AnsweredAt != nil {
		t.Fatalf("historical active evidence survived an earlier terminal boundary: %#v", call)
	}
}

func TestHistoricalCallEventsAfterTunnelFailureCannotReopenCall(t *testing.T) {
	app, err := New(Config{
		DataFile: filepath.Join(t.TempDir(), "state.json"), Telephony: TelephonyConfig{Type: "memory"},
		SIMAuth: SIMAuthConfig{Type: "disabled"}, Voice: VoiceConfig{EngineEventToken: "test-token"},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(app.Handler())
	defer server.Close()

	tunnelUpAt := time.Date(2026, 9, 10, 10, 45, 0, 0, time.UTC)
	callStartedAt := tunnelUpAt.Add(time.Second)
	callActiveAt := tunnelUpAt.Add(1400 * time.Millisecond)
	tunnelDownAt := tunnelUpAt.Add(1200 * time.Millisecond)
	postTunnelEvent(t, server.URL, "process-one", 1, true, tunnelUpAt, http.StatusNoContent)
	postTunnelEvent(t, server.URL, "process-one", 2, false, tunnelDownAt, http.StatusNoContent)

	// Both call hooks arrive only after the durable down has been committed.
	postEngineEvent(t, server.URL, "test-token", map[string]any{
		"instance": "engine-1", "event": "call_active", "call_id": "post-down-call",
		"recorded_at_ns": callActiveAt.UnixNano(), "args": []string{"out", "+15555550108"},
	}, http.StatusOK)
	postEngineEvent(t, server.URL, "test-token", map[string]any{
		"instance": "engine-1", "event": "call_out", "call_id": "post-down-call",
		"recorded_at_ns": callStartedAt.UnixNano(), "args": []string{"+15555550108"},
	}, http.StatusOK)

	call, ok := app.store.GetCallByEngineID("engine-1", "post-down-call")
	if !ok || call.State != "interrupted" || call.EndedAt == nil || !call.EndedAt.Equal(tunnelDownAt) {
		t.Fatalf("historical call hooks reopened a call after tunnel failure: %#v", call)
	}
	if !call.StartedAt.Equal(callStartedAt) || call.AnsweredAt != nil {
		t.Fatalf("late lifecycle refinement produced inconsistent history: %#v", call)
	}
}

func TestCallCanPrecedeDelayedTunnelUpDelivery(t *testing.T) {
	app, err := New(Config{
		DataFile: filepath.Join(t.TempDir(), "state.json"), Telephony: TelephonyConfig{Type: "memory"},
		SIMAuth: SIMAuthConfig{Type: "disabled"}, Voice: VoiceConfig{EngineEventToken: "test-token"},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(app.Handler())
	defer server.Close()

	firstUpAt := time.Date(2026, 9, 10, 11, 0, 0, 0, time.UTC)
	processStartAt := firstUpAt.Add(time.Second)
	secondUpAt := firstUpAt.Add(2 * time.Second)
	callStartedAt := firstUpAt.Add(3 * time.Second)
	callActiveAt := firstUpAt.Add(4 * time.Second)
	postTunnelEvent(t, server.URL, "process-one", 1, true, firstUpAt, http.StatusNoContent)
	postEngineEvent(t, server.URL, "test-token", map[string]any{
		"instance": "engine-1", "event": "tunnel_start", "recorded_at_ns": processStartAt.UnixNano(),
		"tunnel_generation": "process-two", "tunnel_sequence": 1, "args": []string{"replacement process"},
	}, http.StatusNoContent)

	// The direct call evidence overtakes the already-recorded tunnel_up in its
	// durable delivery queue. Since it is newer than the last down boundary, it
	// must remain open while the up webhook catches up.
	postEngineEvent(t, server.URL, "test-token", map[string]any{
		"instance": "engine-1", "event": "call_out", "call_id": "after-down-call",
		"recorded_at_ns": callStartedAt.UnixNano(), "args": []string{"+15555550112"},
	}, http.StatusOK)
	postEngineEvent(t, server.URL, "test-token", map[string]any{
		"instance": "engine-1", "event": "call_active", "call_id": "after-down-call",
		"recorded_at_ns": callActiveAt.UnixNano(), "args": []string{"out", "+15555550112"},
	}, http.StatusOK)
	call, _ := app.store.GetCallByEngineID("engine-1", "after-down-call")
	if call.State != "active" || call.EndedAt != nil {
		t.Fatalf("queued tunnel up caused a false terminal call: %#v", call)
	}
	postTunnelEvent(t, server.URL, "process-two", 2, true, secondUpAt, http.StatusNoContent)
	call, _ = app.store.GetCallByEngineID("engine-1", "after-down-call")
	if call.State != "active" || call.EndedAt != nil {
		t.Fatalf("delayed tunnel up did not preserve the established call: %#v", call)
	}
}

func TestOldCallHookCannotReopenAfterNewTunnelAndManagerRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	cfg := Config{
		DataFile: path, Telephony: TelephonyConfig{Type: "memory"},
		SIMAuth: SIMAuthConfig{Type: "disabled"}, Voice: VoiceConfig{EngineEventToken: "test-token"},
	}
	app, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(app.Handler())
	firstUpAt := time.Date(2026, 9, 10, 11, 30, 0, 0, time.UTC)
	oldCallAt := firstUpAt.Add(time.Second)
	lastDownAt := firstUpAt.Add(2 * time.Second)
	processStartAt := firstUpAt.Add(3 * time.Second)
	secondUpAt := firstUpAt.Add(4 * time.Second)
	postTunnelEvent(t, server.URL, "process-one", 1, true, firstUpAt, http.StatusNoContent)
	postTunnelEvent(t, server.URL, "process-one", 2, false, lastDownAt, http.StatusNoContent)
	postEngineEvent(t, server.URL, "test-token", map[string]any{
		"instance": "engine-1", "event": "tunnel_start", "recorded_at_ns": processStartAt.UnixNano(),
		"tunnel_generation": "process-two", "tunnel_sequence": 1, "args": []string{"replacement process"},
	}, http.StatusNoContent)
	postTunnelEvent(t, server.URL, "process-two", 2, true, secondUpAt, http.StatusNoContent)
	server.Close()

	restarted, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	restartedServer := httptest.NewServer(restarted.Handler())
	defer restartedServer.Close()
	postEngineEvent(t, restartedServer.URL, "test-token", map[string]any{
		"instance": "engine-1", "event": "call_out", "call_id": "old-generation-call",
		"recorded_at_ns": oldCallAt.UnixNano(), "args": []string{"+15555550113"},
	}, http.StatusOK)
	call, _ := restarted.store.GetCallByEngineID("engine-1", "old-generation-call")
	if call.State != "interrupted" || call.EndedAt == nil || !call.EndedAt.Equal(processStartAt) {
		t.Fatalf("old hook reopened after a newer tunnel generation: %#v", call)
	}
}

func TestLegacyReadyCheckpointUsesUpdatedAtAsCausalBoundary(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	boundary := time.Date(2026, 9, 10, 12, 15, 0, 0, time.UTC)
	store.engineTunnels["engine-1"] = EngineTunnelCheckpoint{
		Instance: "engine-1", Generation: "pre-last-down-field", Sequence: 7,
		Ready: true, UpdatedAt: boundary,
	}
	call, _, err := store.ApplyEngineCallEvent(engineCallUpdate{
		ProposedID: "legacy-boundary-call", Instance: "engine-1", EngineCallID: "old-hook",
		Event: "call_out", Direction: DirectionOutgoing, Peer: "+15555550114", At: boundary.Add(-time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	if call.State != "interrupted" || call.EndedAt == nil || !call.EndedAt.Equal(boundary) {
		t.Fatalf("legacy checkpoint admitted a pre-generation hook: %#v", call)
	}
}

func TestPrivateIncomingCallUpdatesByStableCallID(t *testing.T) {
	app, err := New(Config{
		DataFile: filepath.Join(t.TempDir(), "state.json"), Telephony: TelephonyConfig{Type: "memory"},
		SIMAuth: SIMAuthConfig{Type: "disabled"}, Voice: VoiceConfig{EngineEventToken: "test-token"},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(app.Handler())
	defer server.Close()
	startedAt := time.Date(2026, 9, 10, 12, 30, 0, 0, time.UTC)
	base := map[string]any{"instance": "engine-1", "call_id": "private-call"}
	base["event"], base["recorded_at_ns"], base["args"] = "call_in", startedAt.UnixNano(), []string{""}
	postEngineEvent(t, server.URL, "test-token", base, http.StatusOK)
	base["event"], base["recorded_at_ns"], base["args"] = "call_active", startedAt.Add(time.Second).UnixNano(), []string{"in", ""}
	postEngineEvent(t, server.URL, "test-token", base, http.StatusOK)
	base["event"], base["recorded_at_ns"], base["args"] = "call_result", startedAt.Add(time.Minute).UnixNano(), []string{"in", "", "ANSWER", "16"}
	postEngineEvent(t, server.URL, "test-token", base, http.StatusOK)

	call, ok := app.store.GetCallByEngineID("engine-1", "private-call")
	if !ok || call.Peer != "Private number" || call.State != "ended" || call.AnsweredAt == nil || call.EndedAt == nil {
		t.Fatalf("private call did not complete through empty-peer updates: %#v", call)
	}
}

func TestPrivateCallLifecycleRetriesUntilStartIsDurable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	app, err := New(Config{
		DataFile: path, Telephony: TelephonyConfig{Type: "memory"},
		SIMAuth: SIMAuthConfig{Type: "disabled"}, Voice: VoiceConfig{EngineEventToken: "test-token"},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(app.Handler())
	defer server.Close()
	startedAt := time.Date(2026, 9, 10, 12, 45, 0, 0, time.UTC)
	activeAt := startedAt.Add(time.Second)
	endedAt := startedAt.Add(time.Minute)
	base := map[string]any{"instance": "engine-1", "call_id": "retry-private-call"}
	originalPath := app.store.path
	app.store.path = t.TempDir()
	base["event"], base["recorded_at_ns"], base["args"] = "call_in", startedAt.UnixNano(), []string{""}
	postEngineEvent(t, server.URL, "test-token", base, http.StatusInternalServerError)
	base["event"], base["recorded_at_ns"], base["args"] = "call_active", activeAt.UnixNano(), []string{"in", ""}
	postEngineEvent(t, server.URL, "test-token", base, http.StatusConflict)
	base["event"], base["recorded_at_ns"], base["args"] = "call_result", endedAt.UnixNano(), []string{"in", "", "ANSWER", "16"}
	postEngineEvent(t, server.URL, "test-token", base, http.StatusConflict)
	if calls := app.store.Calls(); len(calls) != 0 {
		t.Fatalf("failed private start leaked partial history: %#v", calls)
	}

	// A later outbox pass retries the retained files in source-time order.
	app.store.path = originalPath
	base["event"], base["recorded_at_ns"], base["args"] = "call_in", startedAt.UnixNano(), []string{""}
	postEngineEvent(t, server.URL, "test-token", base, http.StatusOK)
	base["event"], base["recorded_at_ns"], base["args"] = "call_active", activeAt.UnixNano(), []string{"in", ""}
	postEngineEvent(t, server.URL, "test-token", base, http.StatusOK)
	base["event"], base["recorded_at_ns"], base["args"] = "call_result", endedAt.UnixNano(), []string{"in", "", "ANSWER", "16"}
	postEngineEvent(t, server.URL, "test-token", base, http.StatusOK)

	reopened, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	call, ok := reopened.GetCallByEngineID("engine-1", "retry-private-call")
	if !ok || call.Peer != "Private number" || call.State != "ended" || call.EndedAt == nil || !call.EndedAt.Equal(endedAt) {
		t.Fatalf("retained private lifecycle did not converge durably: %#v", call)
	}
}

func TestUnknownEmptyPeerCallUpdateWaitsForStart(t *testing.T) {
	app, err := New(Config{
		DataFile: filepath.Join(t.TempDir(), "state.json"), Telephony: TelephonyConfig{Type: "memory"},
		SIMAuth: SIMAuthConfig{Type: "disabled"}, Voice: VoiceConfig{EngineEventToken: "test-token"},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(app.Handler())
	defer server.Close()
	postEngineEvent(t, server.URL, "test-token", map[string]any{
		"instance": "engine-1", "event": "call_result", "call_id": "unknown-private-call",
		"recorded_at_ns": time.Now().UTC().UnixNano(), "args": []string{"in", "", "NOANSWER", "19"},
	}, http.StatusConflict)
	if calls := app.store.Calls(); len(calls) != 0 {
		t.Fatalf("empty update without a matching call created history: %#v", calls)
	}
}

func TestConcurrentCallHooksAndTunnelFailureConvergeAtomically(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	tunnelUpAt := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	callStartedAt := tunnelUpAt.Add(time.Second)
	callActiveAt := tunnelUpAt.Add(2 * time.Second)
	tunnelDownAt := tunnelUpAt.Add(3 * time.Second)
	if _, _, _, err := store.ApplyEngineTunnelEvent("engine-1", "process-one", 1, true, true, tunnelUpAt); err != nil {
		t.Fatal(err)
	}

	updates := make([]engineCallUpdate, 0, 40)
	for index := 0; index < 20; index++ {
		updates = append(updates,
			engineCallUpdate{ProposedID: "candidate-start", Instance: "engine-1", EngineCallID: "concurrent-call", Event: "call_out", Direction: DirectionOutgoing, Peer: "+15555550109", At: callStartedAt},
			engineCallUpdate{ProposedID: "candidate-active", Instance: "engine-1", EngineCallID: "concurrent-call", Event: "call_active", Direction: DirectionOutgoing, Peer: "+15555550109", At: callActiveAt},
		)
	}
	start := make(chan struct{})
	errors := make(chan error, len(updates)+1)
	var group sync.WaitGroup
	for _, update := range updates {
		update := update
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			_, _, err := store.ApplyEngineCallEvent(update)
			errors <- err
		}()
	}
	group.Add(1)
	go func() {
		defer group.Done()
		<-start
		_, _, _, err := store.ApplyEngineTunnelEvent("engine-1", "process-one", 2, false, false, tunnelDownAt)
		errors <- err
	}()
	close(start)
	group.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}

	calls := store.Calls()
	if len(calls) != 1 || calls[0].State != "interrupted" || calls[0].EndedAt == nil || !calls[0].EndedAt.Equal(tunnelDownAt) {
		t.Fatalf("concurrent call and tunnel events did not converge: %#v", calls)
	}
}

func TestDurableCallResultRetriesAfterPersistenceFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	app, err := New(Config{
		DataFile: path, Telephony: TelephonyConfig{Type: "memory"},
		SIMAuth: SIMAuthConfig{Type: "disabled"}, Voice: VoiceConfig{EngineEventToken: "test-token"},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(app.Handler())
	defer server.Close()
	startedAt := time.Date(2026, 9, 10, 13, 0, 0, 0, time.UTC)
	endedAt := startedAt.Add(time.Minute)
	base := map[string]any{"instance": "engine-1", "call_id": "retry-result"}
	base["event"], base["recorded_at_ns"], base["args"] = "call_out", startedAt.UnixNano(), []string{"+15555550110"}
	postEngineEvent(t, server.URL, "test-token", base, http.StatusOK)

	originalPath := app.store.path
	app.store.path = t.TempDir() // Atomic replacement over a directory must fail.
	base["event"], base["recorded_at_ns"], base["args"] = "call_result", endedAt.UnixNano(), []string{"out", "+15555550110", "ANSWER", "16"}
	postEngineEvent(t, server.URL, "test-token", base, http.StatusInternalServerError)
	call, _ := app.store.GetCallByEngineID("engine-1", "retry-result")
	if call.EndedAt != nil {
		t.Fatalf("failed persistence leaked terminal state into memory: %#v", call)
	}

	app.store.path = originalPath
	postEngineEvent(t, server.URL, "test-token", base, http.StatusOK)
	reopened, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	call, ok := reopened.GetCallByEngineID("engine-1", "retry-result")
	if !ok || call.EndedAt == nil || !call.EndedAt.Equal(endedAt) {
		t.Fatalf("retried terminal result was not durable after restart: %#v", call)
	}
}

func TestSaveCallRollsBackMemoryWhenPersistenceFails(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	startedAt := time.Date(2026, 9, 10, 14, 0, 0, 0, time.UTC)
	call := Call{ID: "rollback-call", Direction: DirectionOutgoing, Peer: "+15555550111", State: "active", StartedAt: startedAt, UpdatedAt: startedAt}
	if err := store.SaveCall(call); err != nil {
		t.Fatal(err)
	}
	store.path = t.TempDir()
	endedAt := startedAt.Add(time.Minute)
	call.State, call.UpdatedAt, call.EndedAt = "ended", endedAt, &endedAt
	if err := store.SaveCall(call); err == nil {
		t.Fatal("SaveCall unexpectedly persisted over a directory")
	}
	stored, _ := store.GetCall(call.ID)
	if stored.State != "active" || stored.EndedAt != nil {
		t.Fatalf("failed SaveCall mutated the in-memory source of truth: %#v", stored)
	}
}

func TestDelayedTerminalCallResultUsesEngineTime(t *testing.T) {
	app, err := New(Config{
		DataFile: filepath.Join(t.TempDir(), "state.json"), Telephony: TelephonyConfig{Type: "memory"},
		SIMAuth: SIMAuthConfig{Type: "disabled"}, Voice: VoiceConfig{EngineEventToken: "test-token"},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(app.Handler())
	defer server.Close()

	startedAt := time.Date(2026, 9, 10, 11, 0, 0, 0, time.UTC)
	endedAt := startedAt.Add(5 * time.Minute)
	base := map[string]any{"instance": "engine-1", "call_id": "durable-result"}
	base["event"], base["recorded_at_ns"], base["args"] = "call_out", startedAt.UnixNano(), []string{"+15555550106"}
	postEngineEvent(t, server.URL, "test-token", base, http.StatusOK)
	base["event"], base["recorded_at_ns"], base["args"] = "call_result", endedAt.UnixNano(), []string{"out", "+15555550106", "ANSWER", "16"}
	postEngineEvent(t, server.URL, "test-token", base, http.StatusOK)

	call, ok := app.store.GetCallByEngineID("engine-1", "durable-result")
	if !ok || call.EndedAt == nil {
		t.Fatalf("terminal result did not finalize the call: %#v", call)
	}
	if !call.StartedAt.Equal(startedAt) || !call.UpdatedAt.Equal(endedAt) || !call.EndedAt.Equal(endedAt) {
		t.Fatalf("delayed lifecycle did not retain engine timestamps: %#v", call)
	}
}

func assertRuntimeReady(t *testing.T, baseURL string, want bool) {
	t.Helper()
	status := runtimeStatusForTest(t, baseURL)
	if status.Ready != want {
		t.Fatalf("ready=%v, want %v: %#v", status.Ready, want, status)
	}
}

func runtimeStatusForTest(t *testing.T, baseURL string) RuntimeStatus {
	t.Helper()
	response, err := http.Get(baseURL + "/api/v1/status")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status endpoint returned %d, want 200", response.StatusCode)
	}
	var status RuntimeStatus
	if err := json.NewDecoder(response.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	return status
}

func postTunnelEvent(t *testing.T, baseURL, generation string, sequence uint64, ready bool, at time.Time, wantStatus int) {
	t.Helper()
	event := "tunnel_down"
	if ready {
		event = "tunnel_up"
	}
	postEngineEvent(t, baseURL, "test-token", map[string]any{
		"instance": "engine-1", "event": event, "args": []string{"test transition"},
		"recorded_at_ns": at.UnixNano(), "tunnel_generation": generation, "tunnel_sequence": sequence,
	}, wantStatus)
}

func TestEngineCallEventsCreateAndFinalizeHistory(t *testing.T) {
	cfg := Config{
		DataFile:  filepath.Join(t.TempDir(), "state.json"),
		Telephony: TelephonyConfig{Type: "memory"},
		SIMAuth:   SIMAuthConfig{Type: "disabled"},
		Voice:     VoiceConfig{EngineEventToken: "test-token"},
	}
	app, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(app.Handler())
	defer server.Close()

	postEngineEvent(t, server.URL, "test-token", map[string]any{
		"instance": "native", "event": "call_in", "args": []string{"anonymous"},
	}, http.StatusOK)
	postEngineEvent(t, server.URL, "test-token", map[string]any{
		"instance": "native", "event": "call_active", "args": []string{"in", "anonymous"},
	}, http.StatusOK)
	active := app.store.Calls()
	if len(active) != 1 || active[0].State != "active" || active[0].AnsweredAt == nil {
		t.Fatalf("call did not become active: %#v", active)
	}
	postEngineEvent(t, server.URL, "test-token", map[string]any{
		"instance": "native", "event": "call_result", "args": []string{"in", "anonymous", "NOANSWER", "19"},
	}, http.StatusOK)

	calls := app.store.Calls()
	if len(calls) != 1 {
		t.Fatalf("got %d calls, want one coalesced call", len(calls))
	}
	if calls[0].State != "missed" || calls[0].EndedAt == nil {
		t.Fatalf("call was not finalized: %#v", calls[0])
	}
}

func TestIncomingEngineEventResolvesContactBeforeScreening(t *testing.T) {
	cfg := Config{
		DataFile:          filepath.Join(t.TempDir(), "state.json"),
		Telephony:         TelephonyConfig{Type: "memory"},
		SIMAuth:           SIMAuthConfig{Type: "disabled"},
		Voice:             VoiceConfig{EngineEventToken: "test-token"},
		ScreeningPipeline: []string{"commercial-screener"},
	}
	app, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.store.ReplaceContactSource("phone-a", "Alice's phone", "ES", 1, []ContactSourceRecord{{
		SourceContactID: "alice", Names: []string{"Alice"}, Numbers: []ContactNumber{{Original: "+34612345678"}},
	}}); err != nil {
		t.Fatal(err)
	}
	claimScreeningService(t, app.integrations, "commercial-screener", "screener-connection", screeningCapabilityOptions{})
	streamID, sequence := app.events.StreamState()
	_, events, cancelEvents := app.events.Subscribe(streamID, sequence)
	defer cancelEvents()
	server := httptest.NewServer(app.Handler())
	defer server.Close()

	postEngineEvent(t, server.URL, "test-token", map[string]any{
		"instance": "native", "call_id": "known-contact-call", "event": "call_in", "args": []string{"+34612345678"},
	}, http.StatusOK)

	offer := waitForScreeningEvent(t, events, "screening.offer")
	payload := offer.Payload.(map[string]any)
	callPayload := payload["call"].(map[string]any)
	if payload["target_service"] != "commercial-screener" || callPayload["contact_id"] == "" || callPayload["contact_name"] != "Alice" {
		t.Fatalf("screening offer did not contain the resolved contact: %#v", payload)
	}
}

func TestDiagnosticIncomingEventBypassesScreening(t *testing.T) {
	cfg := Config{
		DataFile:          filepath.Join(t.TempDir(), "state.json"),
		Telephony:         TelephonyConfig{Type: "memory"},
		SIMAuth:           SIMAuthConfig{Type: "disabled"},
		Voice:             VoiceConfig{EngineEventToken: "test-token"},
		ScreeningPipeline: []string{"commercial-screener"},
	}
	app, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(app.Handler())
	defer server.Close()

	postEngineEvent(t, server.URL, "test-token", map[string]any{
		"instance": "native", "call_id": "diagnostic-call", "event": "call_in",
		"args": []string{"Viti test call", "diagnostic"},
	}, http.StatusOK)

	call := app.store.Calls()[0]
	if call.Screening != nil {
		t.Fatalf("diagnostic call entered the screening pipeline: %#v", call.Screening)
	}
}

func TestEngineCallIDSurvivesLongCallsAndLateEvents(t *testing.T) {
	cfg := Config{
		DataFile: filepath.Join(t.TempDir(), "state.json"), Telephony: TelephonyConfig{Type: "memory"},
		SIMAuth: SIMAuthConfig{Type: "disabled"}, Voice: VoiceConfig{EngineEventToken: "test-token"},
	}
	app, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(app.Handler())
	defer server.Close()

	base := map[string]any{"instance": "engine-1", "call_id": "asterisk-linked-42", "trace_id": "client-trace-99"}
	base["event"], base["args"] = "call_out", []string{"+34123456789"}
	postEngineEvent(t, server.URL, "test-token", base, http.StatusOK)
	call := app.store.Calls()[0]
	call.UpdatedAt = time.Now().UTC().Add(-time.Hour)
	if err := app.store.SaveCall(call); err != nil {
		t.Fatal(err)
	}

	base["event"], base["args"] = "call_active", []string{"out", "+34123456789"}
	postEngineEvent(t, server.URL, "test-token", base, http.StatusOK)
	base["event"], base["args"] = "call_result", []string{"out", "+34123456789", "ANSWER", "16"}
	postEngineEvent(t, server.URL, "test-token", base, http.StatusOK)
	// A delayed active webhook must not resurrect a terminal call.
	base["event"], base["args"] = "call_active", []string{"out", "+34123456789"}
	postEngineEvent(t, server.URL, "test-token", base, http.StatusOK)

	calls := app.store.Calls()
	if len(calls) != 1 || calls[0].TraceID != "client-trace-99" || calls[0].State != "ended" || calls[0].AnsweredAt == nil || calls[0].EndedAt == nil {
		t.Fatalf("stable call id did not coalesce terminal lifecycle: %#v", calls)
	}
}

func TestEngineEventRejectsWrongToken(t *testing.T) {
	cfg := Config{
		DataFile:  filepath.Join(t.TempDir(), "state.json"),
		Telephony: TelephonyConfig{Type: "memory"},
		SIMAuth:   SIMAuthConfig{Type: "disabled"},
		Voice:     VoiceConfig{EngineEventToken: "test-token"},
	}
	app, err := New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(app.Handler())
	defer server.Close()
	postEngineEvent(t, server.URL, "wrong", map[string]any{
		"event": "call_in", "args": []string{"anonymous"},
	}, http.StatusUnauthorized)
}

func TestStoreReconcilesOnlyStructurallyInvalidCalls(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().Add(-5 * time.Minute)
	if err := store.SaveCall(Call{ID: "old", Direction: DirectionIncoming, State: "ringing", StartedAt: old, UpdatedAt: old}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveCall(Call{ID: "invalid", Direction: DirectionOutgoing, State: "ringing", StartedAt: old, UpdatedAt: old}); err != nil {
		t.Fatal(err)
	}
	if err := store.ReconcilePersistedCalls(); err != nil {
		t.Fatal(err)
	}
	call, ok := store.GetCall("old")
	if !ok || call.State != "ringing" || call.EndedAt != nil {
		t.Fatalf("valid old call was closed using age alone: %#v", call)
	}
	if _, ok := store.GetCall("invalid"); ok {
		t.Fatal("structurally invalid outgoing call was not removed")
	}
}

func postEngineEvent(t *testing.T, baseURL, token string, payload any, wantStatus int) {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, baseURL+"/api/engine/event", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Viti-Engine-Token", token)
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != wantStatus {
		t.Fatalf("status %d, want %d", response.StatusCode, wantStatus)
	}
}
