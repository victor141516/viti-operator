package app

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestScreeningPipelineCarriesOutputIntoNextRevision(t *testing.T) {
	application, _ := newIntegrationTestServer(t)
	claimScreeningService(t, application.integrations, "metadata", "metadata-connection", screeningCapabilityOptions{})
	claimScreeningService(t, application.integrations, "voice", "voice-connection", screeningCapabilityOptions{})
	manager := newScreeningManager([]string{"metadata", "voice"}, application.store, application.events, application.integrations, screeningCallbacks{})
	call := saveIncomingScreeningCall(t, application, "call-output")
	streamID, sequence := application.events.StreamState()
	_, events, cancel := application.events.Subscribe(streamID, sequence)
	defer cancel()
	manager.StartIncoming(call)
	waitForScreeningEvent(t, events, "call.incoming")
	firstOffer := waitForScreeningEvent(t, events, "screening.offer")
	if firstOffer.Payload.(map[string]any)["target_service"] != "metadata" {
		t.Fatalf("first offer = %#v", firstOffer)
	}
	receipt, err := manager.Decide("metadata", "metadata-connection", "metadata-next", call.ID, screeningDecisionPayload{
		Revision: 1, Action: "next", Output: json.RawMessage(`{"listed":false}`),
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !receipt.Authoritative || receipt.Revision != 2 {
		t.Fatalf("first receipt = %#v", receipt)
	}
	secondOffer := waitForScreeningEvent(t, events, "screening.offer")
	payload := secondOffer.Payload.(map[string]any)
	if payload["target_service"] != "voice" || payload["revision"] != uint64(2) || payload["context"] != "incoming" {
		t.Fatalf("second offer = %#v", payload)
	}
	if _, err := manager.Decide("voice", "voice-connection", "voice-ring", call.ID, screeningDecisionPayload{Revision: 2, Action: "ring_clients"}, false); err != nil {
		t.Fatal(err)
	}
	ctx, cancelWait := context.WithTimeout(context.Background(), time.Second)
	defer cancelWait()
	if action := manager.WaitInitial(ctx, call.ID); action != "ring_clients" {
		t.Fatalf("initial action = %q", action)
	}
	stored, _ := application.store.GetCall(call.ID)
	if stored.Screening == nil || len(stored.Screening.Pipeline) != 2 || len(stored.Screening.Stages) != 2 ||
		stored.Screening.Stages[0].Outcome != "next" || string(stored.Screening.Stages[0].Updates[0].Output) != `{"listed":false}` ||
		stored.Screening.Stages[1].Outcome != "ring_clients" {
		t.Fatalf("stored screening record = %#v", stored.Screening)
	}
}

func TestScreeningPipelineConsumesCompatibleEarlyDecisionAfterTimeout(t *testing.T) {
	application, _ := newIntegrationTestServer(t)
	claimScreeningService(t, application.integrations, "slow", "slow-connection", screeningCapabilityOptions{DecisionTimeoutMS: 25})
	claimScreeningService(t, application.integrations, "ready", "ready-connection", screeningCapabilityOptions{})
	manager := newScreeningManager([]string{"slow", "ready"}, application.store, application.events, application.integrations, screeningCallbacks{})
	call := saveIncomingScreeningCall(t, application, "call-cached")
	manager.StartIncoming(call)
	receipt, err := manager.Decide("ready", "ready-connection", "ready-early", call.ID, screeningDecisionPayload{Revision: 1, Action: "next"}, false)
	if err != nil || !receipt.Cached || receipt.Authoritative {
		t.Fatalf("early receipt = %#v err=%v", receipt, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if action := manager.WaitInitial(ctx, call.ID); action != "ring_clients" {
		t.Fatalf("initial action = %q", action)
	}
	stored, _ := application.store.GetCall(call.ID)
	if stored.Screening.Stages[0].Outcome != "decision_timeout" || stored.Screening.Stages[1].Outcome != "next" {
		t.Fatalf("cached pipeline outcomes = %#v", stored.Screening.Stages)
	}
}

func TestScreeningHangupHasGlobalPriority(t *testing.T) {
	application, _ := newIntegrationTestServer(t)
	claimScreeningService(t, application.integrations, "first", "first-connection", screeningCapabilityOptions{})
	manager := newScreeningManager([]string{"first"}, application.store, application.events, application.integrations, screeningCallbacks{})
	call := saveIncomingScreeningCall(t, application, "call-global-hangup")
	manager.StartIncoming(call)
	if _, err := manager.Decide("safety", "safety-connection", "stop", call.ID, screeningDecisionPayload{Revision: 1, Action: "hangup"}, true); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if action := manager.WaitInitial(ctx, call.ID); action != "hangup" {
		t.Fatalf("initial action = %q", action)
	}
	stored, _ := application.store.GetCall(call.ID)
	if stored.Screening.Outcome != "hangup" || stored.Screening.TerminatedBy != "safety" {
		t.Fatalf("hangup record = %#v", stored.Screening)
	}
}

func TestScreeningControllerAdvancesActiveCallAndRingsClients(t *testing.T) {
	application, _ := newIntegrationTestServer(t)
	claimScreeningService(t, application.integrations, "voice", "voice-connection", screeningCapabilityOptions{ProcessingTimeoutMS: -1})
	claimScreeningService(t, application.integrations, "sentiment", "sentiment-connection", screeningCapabilityOptions{})
	ringCalls := make(chan Call, 1)
	closedControllers := make(chan string, 1)
	manager := newScreeningManager([]string{"voice", "sentiment"}, application.store, application.events, application.integrations, screeningCallbacks{
		closeController: func(serviceName, callID, _ string) { closedControllers <- serviceName + ":" + callID },
		ringClients: func(_ context.Context, call Call) error {
			ringCalls <- call
			return nil
		},
	})
	call := saveIncomingScreeningCall(t, application, "call-controller")
	manager.StartIncoming(call)
	if _, err := manager.Decide("voice", "voice-connection", "voice-control", call.ID, screeningDecisionPayload{Revision: 1, Action: "take_control"}, false); err != nil {
		t.Fatal(err)
	}
	if err := manager.ControllerAttachAllowed("voice", "voice-connection", call.ID); err != nil {
		t.Fatal(err)
	}
	manager.MediaState(call.ID, "voice", "voice-connection", "media-voice", "controller", "connected")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if action := manager.WaitInitial(ctx, call.ID); action != "take_control" {
		t.Fatalf("initial action = %q", action)
	}
	answeredAt := time.Now().UTC()
	call.State, call.AnsweredAt = "active", &answeredAt
	manager.CallActive(call)
	if _, err := manager.Decide("voice", "voice-connection", "voice-next", call.ID, screeningDecisionPayload{
		Revision: 1, Action: "next", Output: json.RawMessage(`{"voice":true}`),
	}, false); err != nil {
		t.Fatal(err)
	}
	select {
	case closed := <-closedControllers:
		if closed != "voice:"+call.ID {
			t.Fatalf("closed controller = %q", closed)
		}
	case <-time.After(time.Second):
		t.Fatal("controlling media was not closed on next")
	}
	if _, err := manager.Decide("sentiment", "sentiment-connection", "sentiment-ring", call.ID, screeningDecisionPayload{Revision: 2, Action: "ring_clients"}, false); err != nil {
		t.Fatal(err)
	}
	select {
	case rung := <-ringCalls:
		if rung.ID != call.ID {
			t.Fatalf("ring call = %#v", rung)
		}
	case <-time.After(time.Second):
		t.Fatal("active call did not ring clients")
	}
	stored, _ := application.store.GetCall(call.ID)
	if !stored.Screening.Answered || stored.Screening.Stages[0].Outcome != "next" || stored.Screening.Stages[1].Context != "active" {
		t.Fatalf("active screening record = %#v", stored.Screening)
	}
}

func TestScreeningDisconnectFailsOpen(t *testing.T) {
	application, _ := newIntegrationTestServer(t)
	claimScreeningService(t, application.integrations, "only", "only-connection", screeningCapabilityOptions{})
	manager := newScreeningManager([]string{"only"}, application.store, application.events, application.integrations, screeningCallbacks{})
	call := saveIncomingScreeningCall(t, application, "call-disconnect")
	manager.StartIncoming(call)
	manager.ServiceDisconnected("only", "only-connection")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if action := manager.WaitInitial(ctx, call.ID); action != "ring_clients" {
		t.Fatalf("initial action = %q", action)
	}
	stored, _ := application.store.GetCall(call.ID)
	if stored.Screening.Stages[0].Outcome != "service_disconnected" {
		t.Fatalf("disconnect outcome = %#v", stored.Screening.Stages)
	}
}

func TestScreeningEmptyPipelineDoesNotCreateRuntimeState(t *testing.T) {
	application, _ := newIntegrationTestServer(t)
	manager := newScreeningManager(nil, application.store, application.events, application.integrations, screeningCallbacks{})
	call := saveIncomingScreeningCall(t, application, "call-no-pipeline")
	manager.StartIncoming(call)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if action := manager.WaitInitial(ctx, call.ID); action != "ring_clients" {
		t.Fatalf("initial action = %q", action)
	}
	stored, _ := application.store.GetCall(call.ID)
	if stored.Screening != nil {
		t.Fatalf("empty pipeline created screening state: %#v", stored.Screening)
	}
}

func TestScreeningActiveCallAfterRestartFailsOpenOnce(t *testing.T) {
	application, _ := newIntegrationTestServer(t)
	ringCalls := make(chan Call, 2)
	manager := newScreeningManager([]string{"voice"}, application.store, application.events, application.integrations, screeningCallbacks{
		ringClients: func(_ context.Context, call Call) error {
			ringCalls <- call
			return nil
		},
	})
	call := saveIncomingScreeningCall(t, application, "call-restart")
	now := time.Now().UTC()
	call.State, call.AnsweredAt = "active", &now
	call.Screening = &CallScreeningRecord{
		Pipeline: []string{"voice"}, StartedAt: now.Add(-time.Second), ContextRevision: 1,
		CurrentStage: 0, State: "controlled", Answered: true,
	}
	if err := application.store.SaveCall(call); err != nil {
		t.Fatal(err)
	}
	manager.CallActive(call)
	manager.CallActive(call)
	select {
	case rung := <-ringCalls:
		if rung.ID != call.ID {
			t.Fatalf("ring call = %#v", rung)
		}
	case <-time.After(time.Second):
		t.Fatal("surviving active call did not fail open")
	}
	select {
	case duplicate := <-ringCalls:
		t.Fatalf("restart recovery rang twice: %#v", duplicate)
	case <-time.After(25 * time.Millisecond):
	}
	stored, _ := application.store.GetCall(call.ID)
	if stored.Screening == nil || stored.Screening.State != "ringing_clients" ||
		stored.Screening.Reason != "manager_restart_fail_open" {
		t.Fatalf("restart recovery record = %#v", stored.Screening)
	}
}

func claimScreeningService(t *testing.T, registry *integrationRegistry, name, connectionID string, options screeningCapabilityOptions) {
	t.Helper()
	raw, err := json.Marshal(options)
	if err != nil {
		t.Fatal(err)
	}
	if !registry.claim(&integrationConnection{
		id: connectionID, name: name, connectedAt: time.Now().UTC(),
		capabilities: map[string]json.RawMessage{"call.screen": raw},
	}) {
		t.Fatalf("could not claim service %q", name)
	}
}

func saveIncomingScreeningCall(t *testing.T, application *App, id string) Call {
	t.Helper()
	now := time.Now().UTC()
	call := Call{
		ID: id, TraceID: "trace-" + id, EngineCallID: "engine-" + id, Instance: "engine-1",
		Direction: DirectionIncoming, Peer: "+12025550123", State: "ringing", StartedAt: now, UpdatedAt: now,
	}
	if err := application.store.SaveCall(call); err != nil {
		t.Fatal(err)
	}
	return call
}

func waitForScreeningEvent(t *testing.T, events <-chan Event, eventType string) Event {
	t.Helper()
	deadline := time.After(time.Second)
	for {
		select {
		case event := <-events:
			if event.Type == eventType {
				return event
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %s", eventType)
		}
	}
}
