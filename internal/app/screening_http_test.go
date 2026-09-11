package app

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestEngineScreenReturnsFailOpenPipelineAction(t *testing.T) {
	application, err := New(Config{
		DataFile: t.TempDir() + "/state.json", Telephony: TelephonyConfig{Type: "memory"},
		SIMAuth: SIMAuthConfig{Type: "disabled"}, Voice: VoiceConfig{EngineEventToken: "engine-secret"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	call := saveIncomingScreeningCall(t, application, "call-engine-screen")
	application.screening.StartIncoming(call)
	body, _ := json.Marshal(map[string]string{"instance": call.Instance, "call_id": call.EngineCallID})
	request := httptest.NewRequest(http.MethodPost, "/api/engine/screen", bytes.NewReader(body))
	request.Header.Set("X-Viti-Engine-Token", "engine-secret")
	recorder := httptest.NewRecorder()
	application.engineScreen(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("screen status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil || response["action"] != "ring_clients" {
		t.Fatalf("screen response=%#v err=%v", response, err)
	}
}

func TestEngineScreenReturnsPendingWithoutAbandoningPipeline(t *testing.T) {
	application, err := New(Config{
		DataFile: t.TempDir() + "/state.json", Telephony: TelephonyConfig{Type: "memory"},
		SIMAuth: SIMAuthConfig{Type: "disabled"}, Voice: VoiceConfig{EngineEventToken: "engine-secret"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	claimScreeningService(t, application.integrations, "slow", "slow-connection", screeningCapabilityOptions{DecisionTimeoutMS: 2000})
	application.screening = newScreeningManager([]string{"slow"}, application.store, application.events, application.integrations, screeningCallbacks{})
	call := saveIncomingScreeningCall(t, application, "call-engine-pending")
	application.screening.StartIncoming(call)
	body, _ := json.Marshal(map[string]string{"instance": call.Instance, "call_id": call.EngineCallID})
	request := httptest.NewRequest(http.MethodPost, "/api/engine/screen", bytes.NewReader(body))
	ctx, cancel := context.WithTimeout(request.Context(), 10*time.Millisecond)
	defer cancel()
	request = request.WithContext(ctx)
	request.Header.Set("X-Viti-Engine-Token", "engine-secret")
	recorder := httptest.NewRecorder()
	application.engineScreen(recorder, request)
	var response map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil || response["action"] != "pending" {
		t.Fatalf("screen response=%#v err=%v", response, err)
	}
	stored, _ := application.store.GetCall(call.ID)
	if stored.Screening == nil || stored.Screening.State != "screening" {
		t.Fatalf("poll cancellation abandoned pipeline: %#v", stored.Screening)
	}
	application.screening.ServiceDisconnected("slow", "slow-connection")
}

func TestCallListUsesScreeningSummaryAndDetailsRetainHistory(t *testing.T) {
	application, _ := newIntegrationTestServer(t)
	call := saveIncomingScreeningCall(t, application, "call-detail")
	call.Screening = &CallScreeningRecord{
		Pipeline: []string{"one"}, State: "ringing_clients",
		Stages: []ScreeningStageRecord{{Index: 0, ServiceName: "one", Updates: []ScreeningUpdate{{Action: "next"}}}},
	}
	if err := application.store.SaveCall(call); err != nil {
		t.Fatal(err)
	}
	listRecorder := httptest.NewRecorder()
	application.calls(listRecorder, httptest.NewRequest(http.MethodGet, "/api/v1/calls", nil))
	var list map[string][]map[string]any
	if err := json.Unmarshal(listRecorder.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	summary := list["calls"][0]["screening"].(map[string]any)
	if _, leaked := summary["stages"]; leaked {
		t.Fatalf("call list leaked full screening history: %#v", summary)
	}
	detailRequest := httptest.NewRequest(http.MethodGet, "/api/v1/calls/"+call.ID, nil)
	detailRequest.SetPathValue("id", call.ID)
	detailRecorder := httptest.NewRecorder()
	application.callDetails(detailRecorder, detailRequest)
	var detail Call
	if err := json.Unmarshal(detailRecorder.Body.Bytes(), &detail); err != nil || detail.Screening == nil || len(detail.Screening.Stages) != 1 {
		t.Fatalf("call detail=%#v err=%v", detail, err)
	}
}
