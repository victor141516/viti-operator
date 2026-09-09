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
	"time"
)

type serialSMSFixture struct {
	MemoryGateway
	mu      sync.Mutex
	active  int
	maximum int
	started chan struct{}
	release chan struct{}
}

func (fixture *serialSMSFixture) SendSMS(ctx context.Context, _, _ string) (SMSSubmitResult, error) {
	fixture.mu.Lock()
	fixture.active++
	if fixture.active > fixture.maximum {
		fixture.maximum = fixture.active
	}
	fixture.mu.Unlock()
	fixture.started <- struct{}{}
	select {
	case <-fixture.release:
	case <-ctx.Done():
		return SMSSubmitResult{State: "submitted"}, ctx.Err()
	}
	fixture.mu.Lock()
	fixture.active--
	fixture.mu.Unlock()
	return SMSSubmitResult{State: "delivered"}, nil
}

func TestConcurrentSMSRequestsShareHistoryAndSerializeTransport(t *testing.T) {
	cfg := Config{
		DataFile:  filepath.Join(t.TempDir(), "state.json"),
		Telephony: TelephonyConfig{Type: "memory"},
		SIMAuth:   SIMAuthConfig{Type: "disabled"},
	}
	application, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	fixture := &serialSMSFixture{
		started: make(chan struct{}, 2),
		release: make(chan struct{}),
	}
	application.gateway = fixture
	server := httptest.NewServer(application.Handler())
	defer server.Close()

	responses := make(chan error, 2)
	for _, body := range []string{"first", "second"} {
		go func(text string) {
			payload, _ := json.Marshal(map[string]string{"to": "+15555550101", "text": text})
			response, requestErr := http.Post(server.URL+"/api/v1/messages", "application/json", bytes.NewReader(payload))
			if requestErr == nil {
				defer response.Body.Close()
				if response.StatusCode != http.StatusAccepted {
					requestErr = &unexpectedHTTPStatus{got: response.StatusCode, want: http.StatusAccepted}
				}
			}
			responses <- requestErr
		}(body)
	}
	for range 2 {
		if err := <-responses; err != nil {
			t.Fatal(err)
		}
	}

	select {
	case <-fixture.started:
	case <-time.After(time.Second):
		t.Fatal("first SMS did not reach gateway")
	}
	select {
	case <-fixture.started:
		t.Fatal("second SMS entered the single-channel transport concurrently")
	case <-time.After(75 * time.Millisecond):
	}
	close(fixture.release)
	select {
	case <-fixture.started:
	case <-time.After(time.Second):
		t.Fatal("second SMS did not run after the first completed")
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		messages := application.store.Messages()
		if len(messages) == 2 && messages[0].State == "delivered" && messages[1].State == "delivered" {
			fixture.mu.Lock()
			maximum := fixture.maximum
			fixture.mu.Unlock()
			if maximum != 1 {
				t.Fatalf("maximum concurrent sends=%d, want 1", maximum)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("shared history did not contain two delivered messages: %#v", application.store.Messages())
}

type unexpectedHTTPStatus struct {
	got  int
	want int
}

func (status *unexpectedHTTPStatus) Error() string {
	return http.StatusText(status.got) + ", want " + http.StatusText(status.want)
}
