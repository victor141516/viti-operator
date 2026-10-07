package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func probeFixture() map[string]any {
	return map[string]any{
		"client_hash": strings.Repeat("a", 64), "session": "00000000-0000-0000-0000-000000000000",
		"build": "background-probe-20261007", "dispatch_count": float64(3), "event": "registered",
		"attached": false, "js_status_age_ms": float64(-1),
	}
}

func TestVoiceDiagnosticsRemainSafeWithFullRecorderDisabled(t *testing.T) {
	a := &App{}
	data := probeFixture()
	data["password"] = "private-test-secret"
	data["sip"] = "private-test-sip"
	data["event"] = "private-test-phone"
	data["js_status"] = "private-test-exception"
	data["process_importance"] = "private-test-string"
	data["tick_count"] = 2.5
	data["uptime_ms"] = 1e15
	body, _ := json.Marshal(map[string]any{"source": "android-signaling", "kind": "signaling.snapshot", "data": data})
	w := httptest.NewRecorder()
	a.diagnosticEvent(w, httptest.NewRequest(http.MethodPost, "/api/v1/diagnostics/events", bytes.NewReader(body)))
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d", w.Code)
	}
	w = httptest.NewRecorder()
	a.voiceDiagnosticSnapshots(w, httptest.NewRequest(http.MethodGet, "/api/v1/diagnostics/voice", nil))
	if strings.Contains(w.Body.String(), "private-test") {
		t.Fatal("unapproved data escaped the allowlist")
	}
	for _, field := range []string{"event", "js_status", "process_importance", "tick_count", "uptime_ms"} {
		if _, exists := a.voiceDiagnostics.entries[0][field]; exists {
			t.Fatalf("invalid field %s retained", field)
		}
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("probe response may be cached")
	}
}

func TestVoiceDiagnosticsBoundedFilteredAndExpired(t *testing.T) {
	a := &App{}
	for n := 0; n < voiceDiagnosticLimit+5; n++ {
		data := probeFixture()
		data["dispatch_count"] = float64(n)
		if !a.voiceDiagnostics.add(data) {
			t.Fatal("valid snapshot rejected")
		}
	}
	if len(a.voiceDiagnostics.entries) != voiceDiagnosticLimit || a.voiceDiagnostics.entries[0]["dispatch_count"] != int64(5) {
		t.Fatal("retention is not bounded FIFO")
	}
	for n := range a.voiceDiagnostics.entries {
		a.voiceDiagnostics.entries[n]["at"] = time.Now().Add(-7 * time.Hour)
	}
	fresh := probeFixture()
	fresh["client_hash"] = strings.Repeat("b", 64)
	a.voiceDiagnostics.add(fresh)
	for _, test := range []struct {
		filter string
		want   int
	}{{"", 1}, {strings.Repeat("b", 64), 1}, {strings.Repeat("a", 64), 0}} {
		w := httptest.NewRecorder()
		a.voiceDiagnosticSnapshots(w, httptest.NewRequest(http.MethodGet, "/api/v1/diagnostics/voice?client_hash="+test.filter, nil))
		var payload struct {
			Snapshots []map[string]any `json:"snapshots"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		if len(payload.Snapshots) != test.want {
			t.Fatalf("count=%d want=%d", len(payload.Snapshots), test.want)
		}
	}
}

func TestVoiceDiagnosticInvalidIdentityAndAuthorization(t *testing.T) {
	a := &App{cfg: Config{APIKey: "test-key"}}
	for _, h := range []http.Handler{a.authorized(http.HandlerFunc(a.diagnosticEvent)), a.authorized(http.HandlerFunc(a.voiceDiagnosticSnapshots))} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/diagnostics/voice", nil))
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("unauthorized status=%d", w.Code)
		}
	}
	for _, value := range []any{nil, "text", map[string]any{}, map[string]any{"client_hash": "phone", "session": "credential"}} {
		if a.voiceDiagnostics.add(value) {
			t.Fatalf("accepted unsafe identity: %v", fmt.Sprintf("%T", value))
		}
	}
}

func TestNativeVoiceProbeIdentifiesNoWebViewWithoutRetainingPayloads(t *testing.T) {
	a := &App{}
	data := probeFixture()
	data["build"] = "native-voice-20261007"
	data["webview_owned"] = false
	data["socket_state"] = "open"
	data["socket_sent"] = float64(12)
	data["socket_received"] = float64(11)
	data["socket_payload"] = "private-test-sip"
	if !a.voiceDiagnostics.add(data) { t.Fatal("native snapshot rejected") }
	entry := a.voiceDiagnostics.entries[0]
	if entry["build"] != "native-voice-20261007" || entry["webview_owned"] != false || entry["socket_sent"] != int64(12) { t.Fatal("native state was lost") }
	if _, exists := entry["socket_payload"]; exists { t.Fatal("socket payload retained") }
}
