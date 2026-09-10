package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type authReadinessFixture struct {
	err error
}

func (f authReadinessFixture) AuthenticateAKA(context.Context, []byte, []byte) (AKAResult, error) {
	return AKAResult{}, errors.New("not used")
}

func (f authReadinessFixture) CheckSIMReady(context.Context) error {
	return f.err
}

func TestAuthenticateReadyReportsProviderStateWithoutIssuingAChallenge(t *testing.T) {
	for _, test := range []struct {
		name   string
		err    error
		ready  bool
		detail string
	}{
		{name: "ready", ready: true},
		{name: "SIM absent", err: errors.New("OpenStick SIM is not ready: state is ABSENT"), detail: "ABSENT"},
	} {
		t.Run(test.name, func(t *testing.T) {
			application := &App{auth: authReadinessFixture{err: test.err}}
			request := httptest.NewRequest(http.MethodGet, "/api/v1/authenticate/ready", nil)
			response := httptest.NewRecorder()
			application.authenticateReady(response, request)
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d", response.Code)
			}
			var payload struct {
				Ready  bool   `json:"ready"`
				Detail string `json:"detail"`
			}
			if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			if payload.Ready != test.ready || !strings.Contains(payload.Detail, test.detail) {
				t.Fatalf("unexpected readiness payload: %#v", payload)
			}
		})
	}
}

func TestAuthBrokerRoundTrip(t *testing.T) {
	broker := NewAuthBroker()
	resultCh := make(chan AKAResult, 1)
	errCh := make(chan error, 1)
	go func() {
		result, err := broker.Submit(context.Background(), "phone", bytes.Repeat([]byte{1}, 16), bytes.Repeat([]byte{2}, 16), time.Second)
		resultCh <- result
		errCh <- err
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	challenge := broker.Poll(ctx, "phone")
	if challenge == nil {
		t.Fatal("expected challenge")
	}
	response := AuthResponse{
		RES: base64.RawStdEncoding.EncodeToString(bytes.Repeat([]byte{3}, 8)),
		CK:  base64.RawStdEncoding.EncodeToString(bytes.Repeat([]byte{4}, 16)),
		IK:  base64.RawStdEncoding.EncodeToString(bytes.Repeat([]byte{5}, 16)),
	}
	if err := broker.Respond("phone", challenge.ID, response); err != nil {
		t.Fatal(err)
	}
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
	result := <-resultCh
	if len(result.RES) != 8 || len(result.CK) != 16 || len(result.IK) != 16 {
		t.Fatalf("unexpected result lengths: %d/%d/%d", len(result.RES), len(result.CK), len(result.IK))
	}
	if err := broker.Respond("phone", challenge.ID, response); err == nil {
		t.Fatal("expected replay response to fail")
	}
}

func TestAdaptiveAuthenticationPinsPendingChallengeToInitialAgent(t *testing.T) {
	broker := NewAuthBroker()
	selected := "agent-one"
	auth := &AdaptiveAuthenticator{
		Broker: broker, RemoteTimeout: time.Second,
		Select: func(time.Time) SIMAuthSelection { return SIMAuthSelection{Mode: "remote", AgentID: selected} },
	}
	resultCh := make(chan AKAResult, 1)
	errCh := make(chan error, 1)
	go func() {
		result, err := auth.AuthenticateAKA(context.Background(), bytes.Repeat([]byte{1}, 16), bytes.Repeat([]byte{2}, 16))
		resultCh <- result
		errCh <- err
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	challenge := broker.Poll(ctx, "agent-one")
	if challenge == nil {
		t.Fatal("expected challenge for initial agent")
	}
	selected = "agent-two"
	otherCtx, otherCancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer otherCancel()
	if other := broker.Poll(otherCtx, "agent-two"); other != nil {
		t.Fatalf("pending challenge moved to takeover agent: %#v", other)
	}
	response := AuthResponse{
		RES: base64.RawStdEncoding.EncodeToString(bytes.Repeat([]byte{3}, 8)),
		CK:  base64.RawStdEncoding.EncodeToString(bytes.Repeat([]byte{4}, 16)),
		IK:  base64.RawStdEncoding.EncodeToString(bytes.Repeat([]byte{5}, 16)),
	}
	if err := broker.Respond("agent-two", challenge.ID, response); err == nil {
		t.Fatal("takeover agent answered a challenge pinned to the previous provider")
	}
	if err := broker.Respond("agent-one", challenge.ID, response); err != nil {
		t.Fatal(err)
	}
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
	if result := <-resultCh; len(result.RES) != 8 {
		t.Fatalf("unexpected AKA result: %#v", result)
	}
}

func TestAdaptiveAuthenticationFailsFreshChallengeWithoutAProvider(t *testing.T) {
	auth := &AdaptiveAuthenticator{Select: func(time.Time) SIMAuthSelection { return SIMAuthSelection{Mode: "disabled"} }}
	result, err := auth.AuthenticateAKA(context.Background(), bytes.Repeat([]byte{1}, 16), bytes.Repeat([]byte{2}, 16))
	if err == nil || len(result.RES) != 0 || len(result.CK) != 0 || len(result.IK) != 0 || len(result.AUTS) != 0 {
		t.Fatalf("absent provider returned fabricated AKA material: result=%#v error=%v", result, err)
	}
}

func TestParseOpenStickAKA1(t *testing.T) {
	material := append([]byte("AKA1"), 8, 16, 16)
	material = append(material, bytes.Repeat([]byte{1}, 8)...)
	material = append(material, bytes.Repeat([]byte{2}, 16)...)
	material = append(material, bytes.Repeat([]byte{3}, 16)...)
	result, err := parseOpenStickMaterial(material)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.RES) != 8 || len(result.CK) != 16 || len(result.IK) != 16 {
		t.Fatalf("unexpected result lengths: %d/%d/%d", len(result.RES), len(result.CK), len(result.IK))
	}
}

func TestParseOpenStickAKAR(t *testing.T) {
	keys := bytes.Repeat([]byte{7}, 32)
	encoded := []byte(base64.RawStdEncoding.EncodeToString(keys))
	material := append([]byte("AKAR"), 8, byte(len(encoded)))
	material = append(material, bytes.Repeat([]byte{1}, 8)...)
	material = append(material, encoded...)
	result, err := parseOpenStickMaterial(material)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.RES) != 8 || !bytes.Equal(result.CK, keys[:16]) || !bytes.Equal(result.IK, keys[16:]) {
		t.Fatal("unexpected parsed result")
	}
}

func TestOpenStickReadinessChecksSIMAndInstalledBridge(t *testing.T) {
	auth := &OpenStickAuthenticator{runADB: fakeOpenStickReadiness(map[string]fakeADBResult{})}
	if err := auth.CheckSIMReady(context.Background()); err != nil {
		t.Fatalf("ready OpenStick rejected: %v", err)
	}
}

func TestOpenStickReadinessRejectsConnectedStickWithoutReadySIM(t *testing.T) {
	auth := &OpenStickAuthenticator{runADB: fakeOpenStickReadiness(map[string]fakeADBResult{
		"shell getprop gsm.sim.state": {output: "ABSENT"},
	})}
	if err := auth.CheckSIMReady(context.Background()); err == nil || !strings.Contains(err.Error(), "SIM is not ready") {
		t.Fatalf("missing SIM readiness error = %v", err)
	}
}

func TestOpenStickReadinessRejectsMissingBridgeProvider(t *testing.T) {
	auth := &OpenStickAuthenticator{runADB: fakeOpenStickReadiness(map[string]fakeADBResult{
		"shell content query --uri content://lab.codex.simidentity.aka/material": {err: errors.New("provider missing")},
	})}
	if err := auth.CheckSIMReady(context.Background()); err == nil || !strings.Contains(err.Error(), "provider") {
		t.Fatalf("missing bridge readiness error = %v", err)
	}
}

func TestOpenStickReadinessDoesNotProbeDuringAKAActivityLifecycle(t *testing.T) {
	called := false
	auth := &OpenStickAuthenticator{runADB: func(context.Context, ...string) ([]byte, error) {
		called = true
		return nil, errors.New("readiness must not race authentication")
	}}
	auth.mu.Lock()
	defer auth.mu.Unlock()

	if err := auth.CheckSIMReady(context.Background()); err != nil {
		t.Fatalf("in-flight AKA should retain the last ready state: %v", err)
	}
	if called {
		t.Fatal("readiness probe raced the bridge activity lifecycle")
	}
}

func TestOpenStickReadinessKeepsRecentSuccessAcrossProbeTimeout(t *testing.T) {
	auth := &OpenStickAuthenticator{runADB: fakeOpenStickReadiness(map[string]fakeADBResult{})}
	if err := auth.CheckSIMReady(context.Background()); err != nil {
		t.Fatal(err)
	}
	auth.lastCheck = time.Time{}
	auth.runADB = func(context.Context, ...string) ([]byte, error) {
		return nil, context.DeadlineExceeded
	}
	if err := auth.CheckSIMReady(context.Background()); err != nil {
		t.Fatalf("one transient timeout discarded a recent ready state: %v", err)
	}
}

func TestOpenStickReadinessDoesNotDebounceDefinitiveSIMAbsence(t *testing.T) {
	auth := &OpenStickAuthenticator{runADB: fakeOpenStickReadiness(map[string]fakeADBResult{})}
	if err := auth.CheckSIMReady(context.Background()); err != nil {
		t.Fatal(err)
	}
	auth.lastCheck = time.Time{}
	auth.runADB = fakeOpenStickReadiness(map[string]fakeADBResult{
		"shell getprop gsm.sim.state": {output: "ABSENT"},
	})
	if err := auth.CheckSIMReady(context.Background()); err == nil || !strings.Contains(err.Error(), "SIM is not ready") {
		t.Fatalf("definitive absence was hidden by the readiness grace window: %v", err)
	}
}

type fakeADBResult struct {
	output string
	err    error
}

func fakeOpenStickReadiness(overrides map[string]fakeADBResult) func(context.Context, ...string) ([]byte, error) {
	defaults := map[string]fakeADBResult{
		"get-state":                                  {output: "device"},
		"shell getprop gsm.sim.state":                {output: "READY"},
		"shell pm path lab.codex.simidentity":        {output: "package:/data/app/lab.codex.simidentity-1.apk"},
		"shell test -x /data/local/tmp/qmi_auth_aka": {},
		"shell content query --uri content://lab.codex.simidentity.aka/material": {output: "No result found."},
	}
	for command, result := range overrides {
		defaults[command] = result
	}
	return func(_ context.Context, args ...string) ([]byte, error) {
		result, exists := defaults[strings.Join(args, " ")]
		if !exists {
			return nil, errors.New("unexpected fake ADB command")
		}
		return []byte(result.output), result.err
	}
}
