package realtimebot

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestVitiClientRegistersMediaCapabilityAndTracksCallAndAccessLifecycle(t *testing.T) {
	upgrader := websocket.Upgrader{}
	serverErrors := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer viti-key" {
			serverErrors <- &testError{"missing Viti authorization header"}
			response.WriteHeader(http.StatusUnauthorized)
			return
		}
		conn, err := upgrader.Upgrade(response, request, nil)
		if err != nil {
			serverErrors <- err
			return
		}
		defer conn.Close()
		var hello map[string]any
		if err := conn.ReadJSON(&hello); err != nil {
			serverErrors <- err
			return
		}
		capabilities, _ := hello["capabilities"].(map[string]any)
		if hello["type"] != "service.hello" || hello["name"] != "openai-test" || capabilities["call.media"] == nil {
			serverErrors <- &testError{"unexpected service.hello"}
			return
		}
		_ = conn.WriteJSON(map[string]any{"type": "service.ready", "version": 1, "auxiliary_active": false})
		// Inactive services must ignore even a stale or incorrectly queued event.
		_ = conn.WriteJSON(map[string]any{
			"type": "call.active", "payload": map[string]any{"call_id": "ignored-call"},
		})
		_ = conn.WriteJSON(map[string]any{"type": "service.access.updated", "version": 1, "auxiliary_active": true})
		_ = conn.WriteJSON(map[string]any{
			"type": "call.active", "payload": map[string]any{"call_id": "call-1"},
		})
		_ = conn.WriteJSON(map[string]any{"type": "service.access.updated", "version": 1, "auxiliary_active": false})
		ended := time.Now().UTC()
		_ = conn.WriteJSON(map[string]any{
			"type": "call.updated", "payload": map[string]any{"id": "call-1", "state": "ended", "ended_at": ended},
		})
		serverErrors <- nil
		<-request.Context().Done()
	}))
	defer server.Close()

	config := Config{VitiURL: server.URL, VitiAPIKey: "viti-key", ServiceName: "openai-test"}
	ctx, cancel := context.WithCancel(context.Background())
	client, err := dialViti(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer client.close()
	active := make(chan string, 1)
	ended := make(chan string, 1)
	access := make(chan bool, 3)
	serveResult := make(chan error, 1)
	go func() {
		serveResult <- client.serve(ctx, vitiHandlers{
			Active: func(id string) { active <- id }, Ended: func(id string) { ended <- id }, AccessChanged: func(enabled bool) { access <- enabled },
		})
	}()
	if enabled := <-access; enabled {
		t.Fatal("service.ready did not report the inactive state")
	}
	if enabled := <-access; !enabled {
		t.Fatal("service.access.updated did not report activation")
	}
	if id := <-active; id != "call-1" {
		t.Fatalf("active call = %q", id)
	}
	if enabled := <-access; enabled {
		t.Fatal("service.access.updated did not report deactivation")
	}
	if id := <-ended; id != "call-1" {
		t.Fatalf("ended call = %q", id)
	}
	select {
	case id := <-active:
		t.Fatalf("inactive client received call %q", id)
	default:
	}
	if err := <-serverErrors; err != nil {
		t.Fatal(err)
	}
	cancel()
	<-serveResult
}

func TestVitiClientRoutesActiveCallToInactiveScreeningService(t *testing.T) {
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		conn, err := upgrader.Upgrade(response, request, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		var hello map[string]any
		_ = conn.ReadJSON(&hello)
		_ = conn.WriteJSON(map[string]any{"type": "service.ready", "auxiliary_active": false})
		_ = conn.WriteJSON(map[string]any{
			"type": "call.active", "payload": map[string]any{"call_id": "screened-call"},
		})
		<-request.Context().Done()
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	client, err := dialViti(ctx, Config{VitiURL: server.URL, VitiAPIKey: "key", ServiceName: "screening-test"})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	defer client.close()
	active := make(chan string, 1)
	serveResult := make(chan error, 1)
	go func() {
		serveResult <- client.serve(ctx, vitiHandlers{
			Active: func(id string) { active <- id }, ActiveWhileInactive: true,
		})
	}()
	select {
	case id := <-active:
		if id != "screened-call" {
			t.Fatalf("active call = %q", id)
		}
	case <-time.After(time.Second):
		t.Fatal("inactive screening service did not receive call.active")
	}
	cancel()
	<-serveResult
}

func TestVitiCommandWaitsForFinalResult(t *testing.T) {
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		conn, err := upgrader.Upgrade(response, request, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		var hello map[string]any
		_ = conn.ReadJSON(&hello)
		_ = conn.WriteJSON(map[string]any{"type": "service.ready"})
		var command map[string]any
		_ = conn.ReadJSON(&command)
		commandID := command["command_id"].(string)
		_ = conn.WriteJSON(map[string]any{"type": "command.result", "command_id": commandID, "status": "accepted"})
		_ = conn.WriteJSON(map[string]any{
			"type": "command.result", "command_id": commandID, "status": "completed",
			"payload": map[string]any{"session_id": "media-1", "sdp_answer": "v=0\r\n"},
		})
		<-request.Context().Done()
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client, err := dialViti(ctx, Config{VitiURL: server.URL, VitiAPIKey: "key", ServiceName: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer client.close()
	serveResult := make(chan error, 1)
	go func() { serveResult <- client.serve(ctx, vitiHandlers{}) }()
	payload, err := client.command(ctx, "media.attach", "call-1", map[string]any{"sdp_offer": "v=0\r\n"})
	if err != nil {
		t.Fatal(err)
	}
	var description mediaDescription
	if err := json.Unmarshal(payload, &description); err != nil || description.SessionID != "media-1" {
		t.Fatalf("description = %#v, error = %v", description, err)
	}
	cancel()
	<-serveResult
}

func TestVitiClientRoutesDelegationAndParticipantEvents(t *testing.T) {
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		conn, err := upgrader.Upgrade(response, request, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		var hello map[string]any
		_ = conn.ReadJSON(&hello)
		_ = conn.WriteJSON(map[string]any{"type": "service.ready", "auxiliary_active": true})
		_ = conn.WriteJSON(map[string]any{
			"type": "screening.delegated", "payload": map[string]any{
				"call_id": "call-commercial", "output": map[string]any{"target_service": "commercial-handler"},
			},
		})
		_ = conn.WriteJSON(map[string]any{
			"type": "call.participant.joined", "payload": map[string]any{
				"call_id": "call-commercial", "service_name": "commercial-handler",
			},
		})
		_ = conn.WriteJSON(map[string]any{
			"type": "call.participant.left", "payload": map[string]any{
				"call_id": "call-commercial", "service_name": "commercial-handler",
			},
		})
		_ = conn.WriteJSON(map[string]any{
			"type": "screening.delegation.ended", "payload": map[string]any{
				"call_id": "call-commercial", "outcome": "ring_clients",
			},
		})
		<-request.Context().Done()
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	client, err := dialViti(ctx, Config{VitiURL: server.URL, VitiAPIKey: "key", ServiceName: "commercial-handler"})
	if err != nil {
		t.Fatal(err)
	}
	defer client.close()
	delegated := make(chan string, 1)
	joined := make(chan string, 1)
	left := make(chan string, 1)
	ended := make(chan string, 1)
	serveResult := make(chan error, 1)
	go func() {
		serveResult <- client.serve(ctx, vitiHandlers{
			Delegated:         func(callID, target string) { delegated <- callID + ":" + target },
			ParticipantJoined: func(callID, service string) { joined <- callID + ":" + service },
			ParticipantLeft:   func(callID, service string) { left <- callID + ":" + service },
			DelegationEnded:   func(callID string) { ended <- callID },
		})
	}()
	if value := <-delegated; value != "call-commercial:commercial-handler" {
		t.Fatalf("delegation = %q", value)
	}
	if value := <-joined; value != "call-commercial:commercial-handler" {
		t.Fatalf("participant = %q", value)
	}
	if value := <-left; value != "call-commercial:commercial-handler" {
		t.Fatalf("participant left = %q", value)
	}
	if value := <-ended; value != "call-commercial" {
		t.Fatalf("delegation ended = %q", value)
	}
	cancel()
	<-serveResult
}

func TestSnapshotDelegationTargetUsesOnlyActiveDelegation(t *testing.T) {
	call := vitiCall{Screening: &vitiScreeningSnapshot{State: "delegated"}}
	call.Screening.Stages = append(call.Screening.Stages, struct {
		Updates []struct {
			Action string          `json:"action"`
			Output json.RawMessage `json:"output,omitempty"`
		} `json:"updates,omitempty"`
	}{})
	call.Screening.Stages[0].Updates = append(call.Screening.Stages[0].Updates, struct {
		Action string          `json:"action"`
		Output json.RawMessage `json:"output,omitempty"`
	}{Action: "delegate", Output: json.RawMessage(`{"target_service":"handler"}`)})
	if target := snapshotDelegationTarget(call); target != "handler" {
		t.Fatalf("target = %q", target)
	}
	call.Screening.State = "ringing_clients"
	if target := snapshotDelegationTarget(call); target != "" {
		t.Fatalf("inactive delegation target = %q", target)
	}
}

func TestIntegrationURLReplacesQueryAndFragment(t *testing.T) {
	endpoint, err := integrationURL("http://manager:8787/root?secret=value#fragment")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(endpoint, "secret") || strings.Contains(endpoint, "fragment") {
		t.Fatalf("integration endpoint retained unrelated URL data: %s", endpoint)
	}
}
