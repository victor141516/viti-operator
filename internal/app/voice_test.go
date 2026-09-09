package app

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestEmbeddedCallUIIncludesLocalMicrophoneMuteControl(t *testing.T) {
	index, err := webAssets.ReadFile("web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range [][]byte{
		[]byte(`id="mute-call"`),
		[]byte(`aria-pressed="false"`),
		[]byte(`<span>Mute</span>`),
	} {
		if !bytes.Contains(index, fragment) {
			t.Fatalf("embedded call UI is missing %q", fragment)
		}
	}
}

func TestVoiceProvisioningDerivesTailscaleFacingURLFromRequest(t *testing.T) {
	cfg := Config{
		Listen:    "0.0.0.0:8787",
		DataFile:  filepath.Join(t.TempDir(), "state.json"),
		Telephony: TelephonyConfig{Type: "memory"},
		SIMAuth:   SIMAuthConfig{Type: "disabled"},
		Voice:     VoiceConfig{Enabled: true, Username: "viti", Password: "secret"},
	}
	app, err := New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "http://100.64.0.10:8787/api/v1/voice", nil)
	response := httptest.NewRecorder()
	app.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status %d", response.Code)
	}
	var body struct {
		WebSocketURL string `json:"websocket_url"`
		Domain       string `json:"domain"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.WebSocketURL != "ws://100.64.0.10:8787/sipws" || body.Domain != "100.64.0.10" {
		t.Fatalf("unexpected provisioning: %+v", body)
	}
}

func TestVoiceProvisioningUsesWSSBehindHTTPSProxy(t *testing.T) {
	cfg := Config{
		Listen:    "0.0.0.0:8787",
		DataFile:  filepath.Join(t.TempDir(), "state.json"),
		Telephony: TelephonyConfig{Type: "memory"},
		SIMAuth:   SIMAuthConfig{Type: "disabled"},
		Voice:     VoiceConfig{Enabled: true, Username: "viti", Password: "secret"},
	}
	app, err := New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "http://viti.tailnet.test/api/v1/voice", nil)
	request.Header.Set("X-Forwarded-Proto", "https")
	response := httptest.NewRecorder()
	app.Handler().ServeHTTP(response, request)
	var body map[string]any
	_ = json.Unmarshal(response.Body.Bytes(), &body)
	if body["websocket_url"] != "wss://viti.tailnet.test/sipws" {
		t.Fatalf("unexpected websocket URL: %v", body["websocket_url"])
	}
}
