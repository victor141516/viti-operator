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

func TestClassifierUsesAudioInputAndTextOnlyOutput(t *testing.T) {
	upgrader := websocket.Upgrader{}
	serverErrors := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		conn, err := upgrader.Upgrade(response, request, nil)
		if err != nil {
			serverErrors <- err
			return
		}
		defer conn.Close()
		var update map[string]any
		if err := conn.ReadJSON(&update); err != nil {
			serverErrors <- err
			return
		}
		encoded, _ := json.Marshal(update)
		text := string(encoded)
		if !strings.Contains(text, `"output_modalities":["text"]`) || !strings.Contains(text, `"type":"audio/pcmu"`) {
			serverErrors <- &testError{"classifier session was not text-only PCMU: " + text}
			return
		}
		if err := conn.WriteJSON(map[string]any{"type": "session.updated"}); err != nil {
			serverErrors <- err
			return
		}
		var input map[string]any
		if err := conn.ReadJSON(&input); err != nil {
			serverErrors <- err
			return
		}
		if input["type"] != "input_audio_buffer.append" {
			serverErrors <- &testError{"classifier did not send input audio"}
			return
		}
		_ = conn.WriteJSON(map[string]any{"type": "response.output_text.delta", "delta": "UNCERTAIN"})
		_ = conn.WriteJSON(map[string]any{"type": "response.done"})
		_ = conn.WriteJSON(map[string]any{"type": "response.output_text.delta", "delta": "COMMERCIAL"})
		_ = conn.WriteJSON(map[string]any{"type": "response.done"})
		serverErrors <- nil
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	session, err := connectClassifier(ctx, Config{
		OpenAIURL: strings.Replace(server.URL, "http://", "ws://", 1), OpenAIAPIKey: "key",
		Model: "gpt-realtime-mini", Instructions: "Classify the caller.",
	})
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan commercialClassification, 1)
	go func() {
		classification, _ := session.classify(ctx)
		result <- classification
	}()
	if err := session.appendAudio(make([]byte, 160)); err != nil {
		t.Fatal(err)
	}
	if classification := <-result; classification != classificationCommercial {
		t.Fatalf("classification = %q", classification)
	}
	if err := <-serverErrors; err != nil {
		t.Fatal(err)
	}
}

func TestParseCommercialClassificationChecksNegativeFirst(t *testing.T) {
	if got := parseCommercialClassification("NON_COMMERCIAL"); got != classificationNonCommercial {
		t.Fatalf("classification = %q", got)
	}
}
