package realtimebot

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestCommercialScreeningKnownContactRingsWithoutMediaOrOpenAI(t *testing.T) {
	upgrader := websocket.Upgrader{}
	commandReceived := make(chan struct{})
	serverErrors := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
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
		if capabilities["call.screen"] == nil || capabilities["call.media"] == nil {
			serverErrors <- &testError{"screening bot did not advertise screening and media"}
			return
		}
		_ = conn.WriteJSON(map[string]any{"type": "service.ready", "auxiliary_active": false})
		_ = conn.WriteJSON(map[string]any{
			"type": "screening.offer", "payload": map[string]any{
				"call_id": "call-known", "target_service": "commercial-screening", "revision": 1, "context": "incoming",
				"call": map[string]any{"call_id": "call-known", "contact_id": "contact-1", "contact_name": "Known caller"},
			},
		})
		var command map[string]any
		if err := conn.ReadJSON(&command); err != nil {
			serverErrors <- err
			return
		}
		payload, _ := command["payload"].(map[string]any)
		output, _ := payload["output"].(map[string]any)
		if command["type"] != "screening.decision" || payload["action"] != "ring_clients" || output["classification"] != "known_contact" {
			encoded, _ := json.Marshal(command)
			serverErrors <- &testError{"unexpected known-contact command: " + string(encoded)}
			return
		}
		commandID, _ := command["command_id"].(string)
		_ = conn.WriteJSON(map[string]any{"type": "command.result", "command_id": commandID, "status": "completed", "payload": map[string]any{"revision": 2, "state": "ringing_clients"}})
		close(commandReceived)
		serverErrors <- nil
		<-request.Context().Done()
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	bot := NewCommercialScreeningBot(ScreeningConfig{
		VitiURL: server.URL, VitiAPIKey: "key", OpenAIURL: "ws://127.0.0.1:1", Model: "gpt-realtime-mini",
		ServiceName: "commercial-screening", HandlerServiceName: "handler", GreetingPath: "missing.wav",
		DecisionTimeout: time.Second, MediaTimeout: time.Second, ClassificationTimeout: time.Second, HandlerJoinTimeout: time.Second,
	}, nil)
	result := make(chan error, 1)
	go func() { result <- bot.runConnection(ctx) }()
	select {
	case <-commandReceived:
	case <-time.After(time.Second):
		t.Fatal("known contact was not passed to clients")
	}
	if err := <-serverErrors; err != nil {
		t.Fatal(err)
	}
	cancel()
	<-result
}
