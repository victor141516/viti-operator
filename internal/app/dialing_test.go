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
	"testing"
)

func TestNormalizeDialDestination(t *testing.T) {
	tests := []struct {
		name, input, country, want string
	}{
		{"Spanish national", "612 345 678", "ES", "+34612345678"},
		{"British trunk prefix", "020 7946 0018", "GB", "+442079460018"},
		{"Italian significant zero", "06 6982 0000", "IT", "+390669820000"},
		{"explicit international", "+1 (202) 555-0123", "ES", "+12025550123"},
		{"European IDD", "00 44 20 7946 0018", "ES", "+442079460018"},
		{"North American IDD", "011 44 20 7946 0018", "US", "+442079460018"},
		{"Australian IDD", "0011 44 20 7946 0018", "AU", "+442079460018"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := normalizeDialDestination(test.input, test.country)
			if err != nil || got != test.want {
				t.Fatalf("normalizeDialDestination(%q, %q) = %q, %v; want %q", test.input, test.country, got, err, test.want)
			}
		})
	}
}

func TestNormalizeDialDestinationRejectsAmbiguousAndUnsupportedInput(t *testing.T) {
	tests := []struct {
		name, input, country string
		want                 error
	}{
		{"national without country", "612345678", "", errDialCountryRequired},
		{"national with invalid country", "612345678", "ZZ", errDialCountryRequired},
		{"emergency number", "112", "ES", errDialUnsupported},
		{"short code", "1004", "ES", errDialUnsupported},
		{"private service code", "*44", "ES", errDialUnsupported},
		{"hash service code", "#31#612345678", "ES", errDialUnsupported},
		{"extension", "+34612345678 ext 4", "ES", errDialInvalid},
		{"pause", "+34612345678,4", "ES", errDialInvalid},
		{"wait", "+34612345678;4", "ES", errDialInvalid},
		{"tel URI", "tel:+34612345678", "ES", errDialInvalid},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got, err := normalizeDialDestination(test.input, test.country); !errors.Is(err, test.want) || got != "" {
				t.Fatalf("normalizeDialDestination(%q, %q) = %q, %v; want %v", test.input, test.country, got, err, test.want)
			}
		})
	}
}

func TestDialNormalizationEndpointAndLegacyStartCallBody(t *testing.T) {
	application := newDialingTestApp(t)
	server := httptest.NewServer(application.Handler())
	defer server.Close()

	payload, _ := json.Marshal(map[string]string{"number": "020 7946 0018", "country": "GB"})
	response, err := http.Post(server.URL+"/api/v1/calls/normalize", "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var normalized map[string]string
	if err := json.NewDecoder(response.Body).Decode(&normalized); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || normalized["number"] != "+442079460018" {
		t.Fatalf("normalize response = %d %#v", response.StatusCode, normalized)
	}

	legacyBody := bytes.NewBufferString(`{"to":"+12025550123"}`)
	legacyResponse, err := http.Post(server.URL+"/api/v1/calls", "application/json", legacyBody)
	if err != nil {
		t.Fatal(err)
	}
	defer legacyResponse.Body.Close()
	if legacyResponse.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(legacyResponse.Body)
		t.Fatalf("legacy start-call status = %d, body = %s", legacyResponse.StatusCode, body)
	}
}

func TestStartCallNormalizesNationalDestinationBeforePersistence(t *testing.T) {
	application := newDialingTestApp(t)
	server := httptest.NewServer(application.Handler())
	defer server.Close()
	payload := bytes.NewBufferString(`{"to":"020 7946 0018","country":"GB"}`)
	response, err := http.Post(server.URL+"/api/v1/calls", "application/json", payload)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var call Call
	if err := json.NewDecoder(response.Body).Decode(&call); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusCreated || call.Peer != "+442079460018" {
		t.Fatalf("start-call response = %d %#v", response.StatusCode, call)
	}
	stored, ok := application.store.GetCall(call.ID)
	if !ok || stored.Peer != call.Peer {
		t.Fatalf("persisted call = %#v, found=%v", stored, ok)
	}
}

func TestDialNormalizationFailureDoesNotCreateCall(t *testing.T) {
	application := newDialingTestApp(t)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/calls/normalize", bytes.NewBufferString(`{"number":"612345678"}`))
	application.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest || !bytes.Contains(recorder.Body.Bytes(), []byte("full international number")) {
		t.Fatalf("failure response = %d %s", recorder.Code, recorder.Body.String())
	}
	if calls := application.store.Calls(); len(calls) != 0 {
		t.Fatalf("normalization failure created calls: %#v", calls)
	}
}

func newDialingTestApp(t *testing.T) *App {
	t.Helper()
	application, err := New(Config{
		DataFile:  filepath.Join(t.TempDir(), "state.json"),
		Telephony: TelephonyConfig{Type: "memory"},
		SIMAuth:   SIMAuthConfig{Type: "disabled"},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	application.gateway = dialingCaptureGateway{MemoryGateway: MemoryGateway{}}
	return application
}

type dialingCaptureGateway struct{ MemoryGateway }

func (dialingCaptureGateway) StartCall(context.Context, string, string) error { return nil }
