package app

import (
	"encoding/json"
	"net/http"
	"regexp"
	"sync"
	"time"
)

const voiceDiagnosticLimit = 256

// This bounded, volatile probe is independent of the full development recorder.
// Never retain arbitrary client payloads, SIP messages, addresses or credentials.
type voiceDiagnosticBuffer struct {
	mu      sync.Mutex
	entries []map[string]any
}

var probeHash = regexp.MustCompile(`^[a-f0-9]{64}$`)
var probeSession = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)

func (b *voiceDiagnosticBuffer) add(value any) bool {
	data, ok := value.(map[string]any)
	if !ok {
		return false
	}
	client, _ := data["client_hash"].(string)
	session, _ := data["session"].(string)
	if !probeHash.MatchString(client) || !probeSession.MatchString(session) {
		return false
	}
	safe := map[string]any{"at": time.Now().UTC(), "client_hash": client, "session": session}
	if data["build"] == "background-probe-20261007" || data["build"] == "native-signaling-20261007" || data["build"] == "native-voice-20261007" {
		safe["build"] = data["build"]
	}
	for _, name := range []string{"tick_count", "dispatch_count", "uptime_ms", "js_status_age_ms", "lease_remaining_ms", "event_age_ms", "process_importance", "window_visibility", "socket_sent", "socket_received"} {
		if n, ok := data[name].(float64); ok && n >= -1 && n <= 1e12 && n == float64(int64(n)) {
			safe[name] = int64(n)
		}
	}
	if v, ok := data["socket_state"].(string); ok && (v == "open" || v == "closed" || v == "connecting") {
		safe["socket_state"] = v
	}
	for _, name := range []string{"enabled", "page_ready", "attached", "wake_lock", "lease_valid", "api_reachable", "voice_registered", "webview_owned"} {
		if v, ok := data[name].(bool); ok {
			safe[name] = v
		}
	}
	for _, name := range []string{"event", "js_status"} {
		if v, ok := data[name].(string); ok {
			switch v {
			case "none", "connected", "disconnected", "registered", "unregistered", "registration-failed", "signaling-status", "not_registered", "disabled", "maintenance", "maintenance-failed":
				safe[name] = v
			}
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.entries) == voiceDiagnosticLimit {
		copy(b.entries, b.entries[1:])
		b.entries = b.entries[:voiceDiagnosticLimit-1]
	}
	b.entries = append(b.entries, safe)
	return true
}

func (a *App) voiceDiagnosticSnapshots(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	entries := make([]map[string]any, 0)
	a.voiceDiagnostics.mu.Lock()
	for _, entry := range a.voiceDiagnostics.entries {
		if time.Since(entry["at"].(time.Time)) > 6*time.Hour {
			continue
		}
		if filter := r.URL.Query().Get("client_hash"); filter != "" && entry["client_hash"] != filter {
			continue
		}
		entries = append(entries, entry)
	}
	a.voiceDiagnostics.mu.Unlock()
	_ = json.NewEncoder(w).Encode(map[string]any{"snapshots": entries})
}
