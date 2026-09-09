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
	started := time.Now().UTC()
	if err := app.store.SaveCall(Call{ID: "active-before-disconnect", Direction: DirectionOutgoing, Peer: "+15555550101", State: "active", StartedAt: started, UpdatedAt: started}); err != nil {
		t.Fatal(err)
	}
	postEngineEvent(t, server.URL, "test-token", map[string]any{"event": "tunnel_down", "args": []string{"test disconnect"}}, http.StatusNoContent)
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
	postEngineEvent(t, server.URL, "test-token", map[string]any{"event": "tunnel_up", "args": []string{"test reconnect"}}, http.StatusNoContent)
	assertRuntimeReady(t, server.URL, true)
}

func assertRuntimeReady(t *testing.T, baseURL string, want bool) {
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
	if status.Ready != want {
		t.Fatalf("ready=%v, want %v: %#v", status.Ready, want, status)
	}
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

func TestStoreClosesStaleOpenCalls(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().Add(-5 * time.Minute)
	if err := store.SaveCall(Call{ID: "old", Direction: DirectionIncoming, State: "ringing", StartedAt: old, UpdatedAt: old}); err != nil {
		t.Fatal(err)
	}
	if err := store.CloseStaleCalls(time.Now().UTC().Add(-2 * time.Minute)); err != nil {
		t.Fatal(err)
	}
	call, ok := store.GetCall("old")
	if !ok || call.State != "interrupted" || call.EndedAt == nil {
		t.Fatalf("stale call was not reconciled: %#v", call)
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
