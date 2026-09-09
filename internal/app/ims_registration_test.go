package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
)

type imsRegistrationFixture struct {
	MemoryGateway
	mu      sync.Mutex
	changes []bool
}

func (f *imsRegistrationFixture) SetIMSRegistration(_ context.Context, enabled bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.changes = append(f.changes, enabled)
	return nil
}

func TestIMSRegistrationPreferenceCanBeChangedAndPersists(t *testing.T) {
	dataFile := filepath.Join(t.TempDir(), "state.json")
	cfg := Config{DataFile: dataFile, Telephony: TelephonyConfig{Type: "memory"}, SIMAuth: SIMAuthConfig{Type: "disabled"}}
	application, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	fixture := &imsRegistrationFixture{}
	application.gateway = fixture
	server := httptest.NewServer(application.Handler())
	defer server.Close()

	payload, _ := json.Marshal(map[string]bool{"enabled": false})
	request, _ := http.NewRequest(http.MethodPut, server.URL+"/api/v1/ims-registration", bytes.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200", response.StatusCode)
	}
	if application.store.IMSRegistrationEnabled() {
		t.Fatal("IMS registration preference remained enabled")
	}
	fixture.mu.Lock()
	changes := append([]bool(nil), fixture.changes...)
	fixture.mu.Unlock()
	if len(changes) != 1 || changes[0] {
		t.Fatalf("registration changes=%v, want [false]", changes)
	}

	reopened, err := OpenStore(dataFile)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.IMSRegistrationEnabled() {
		t.Fatal("disabled IMS registration preference was not persisted")
	}
}
