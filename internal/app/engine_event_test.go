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

	assertRuntimeReady(t, server.URL, false)
	readiness.ready = true
	assertRuntimeReady(t, server.URL, true)
	postTunnelEvent(t, server.URL, "process-one", 1, true, time.Now().UTC(), http.StatusNoContent)
	started := time.Now().UTC()
	if err := app.store.SaveCall(Call{ID: "active-before-disconnect", Direction: DirectionOutgoing, Peer: "+15555550101", State: "active", StartedAt: started, UpdatedAt: started}); err != nil {
		t.Fatal(err)
	}

	readiness.ready = false
	status := runtimeStatusForTest(t, server.URL)
	if status.Ready || status.SIMReady || !status.TunnelReady || !status.IMSReady {
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
	postTunnelEvent(t, server.URL, "modern", 1, true, now.Add(time.Second), http.StatusNoContent)
	postTunnelEvent(t, server.URL, "modern", 2, false, now.Add(2*time.Second), http.StatusNoContent)
	legacy("tunnel_up", now.Add(3*time.Second))
	checkpoint, ok := app.store.EngineTunnelCheckpoint("engine-1")
	if !ok || !checkpoint.Ready || !strings.HasPrefix(checkpoint.Generation, "legacy:") {
		t.Fatalf("legacy rollback did not reclaim tunnel ownership: %#v", checkpoint)
	}
	if err := app.store.SaveCall(Call{ID: "legacy-call", Direction: DirectionOutgoing, Peer: "+15555550103", State: "active", StartedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	legacy("tunnel_down", now.Add(4*time.Second))
	call, _ := app.store.GetCall("legacy-call")
	if call.EndedAt == nil || call.State != "interrupted" {
		t.Fatalf("legacy down event was not correlated to its rollback era: %#v", call)
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
