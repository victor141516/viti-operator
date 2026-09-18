package realtimebot

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestRealtimeSessionBridgesPCMUWithoutTranscoding(t *testing.T) {
	upgrader := websocket.Upgrader{}
	serverErrors := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer test-openai-key" {
			serverErrors <- &testError{"missing OpenAI authorization header"}
			response.WriteHeader(http.StatusUnauthorized)
			return
		}
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
		if !strings.Contains(text, `"type":"session.update"`) || strings.Count(text, `"type":"audio/pcmu"`) != 2 || !strings.Contains(text, `"model":"gpt-realtime"`) {
			serverErrors <- &testError{"unexpected session update: " + text}
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
			serverErrors <- &testError{"unexpected input event"}
			return
		}
		decoded, err := base64.StdEncoding.DecodeString(input["audio"].(string))
		if err != nil || len(decoded) != 160 {
			serverErrors <- &testError{"input audio was not passed through"}
			return
		}
		output := base64.StdEncoding.EncodeToString(make([]byte, 320))
		if err := conn.WriteJSON(map[string]any{
			"type": "response.output_audio.delta", "item_id": "item-1", "content_index": 0, "delta": output,
		}); err != nil {
			serverErrors <- err
			return
		}
		time.Sleep(50 * time.Millisecond)
		serverErrors <- nil
	}))
	defer server.Close()

	config := Config{
		OpenAIURL: strings.Replace(server.URL, "http://", "ws://", 1), OpenAIAPIKey: "test-openai-key",
		Model: "gpt-realtime", Voice: "marin", Instructions: "Keep it short.",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	session, err := connectRealtime(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	writer := &recordingSampleWriter{}
	player := newPCMUPlayer(writer)
	defer player.close()
	runResult := make(chan error, 1)
	go func() { runResult <- session.run(ctx, player) }()
	if err := session.appendAudio(make([]byte, 160)); err != nil {
		t.Fatal(err)
	}
	if err := <-serverErrors; err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		writer.mu.Lock()
		count := len(writer.samples)
		writer.mu.Unlock()
		if count >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("OpenAI output audio was not paced into the media track")
		}
		time.Sleep(10 * time.Millisecond)
	}
	session.close()
	<-runResult
}

func TestRealtimeSessionClearsAndTruncatesInterruptedOutput(t *testing.T) {
	upgrader := websocket.Upgrader{}
	truncateEvents := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		conn, err := upgrader.Upgrade(response, request, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		var update map[string]any
		_ = conn.ReadJSON(&update)
		_ = conn.WriteJSON(map[string]any{"type": "session.updated"})
		_ = conn.WriteJSON(map[string]any{
			"type": "response.output_audio.delta", "item_id": "assistant-item", "content_index": 0,
			"delta": base64.StdEncoding.EncodeToString(make([]byte, 3200)),
		})
		_ = conn.WriteJSON(map[string]any{"type": "input_audio_buffer.speech_started"})
		var truncate map[string]any
		if conn.ReadJSON(&truncate) == nil {
			truncateEvents <- truncate
		}
		_ = conn.WriteJSON(map[string]any{"type": "error", "error": map[string]any{"message": "test complete"}})
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	session, err := connectRealtime(ctx, Config{
		OpenAIURL: strings.Replace(server.URL, "http://", "ws://", 1), OpenAIAPIKey: "key",
		Model: "gpt-realtime", Voice: "marin", Instructions: "Test.",
	})
	if err != nil {
		t.Fatal(err)
	}
	player := newPCMUPlayer(&recordingSampleWriter{})
	defer player.close()
	runResult := make(chan error, 1)
	go func() { runResult <- session.run(ctx, player) }()
	select {
	case event := <-truncateEvents:
		if event["type"] != "conversation.item.truncate" || event["item_id"] != "assistant-item" {
			t.Fatalf("truncate event = %#v", event)
		}
		if end, ok := event["audio_end_ms"].(float64); !ok || end < 0 || end > 400 {
			t.Fatalf("audio_end_ms = %#v", event["audio_end_ms"])
		}
	case <-ctx.Done():
		t.Fatal("interruption did not produce conversation.item.truncate")
	}
	<-runResult
}

func TestConnectRealtimeCancellationClosesHandshakeImmediately(t *testing.T) {
	upgrader := websocket.Upgrader{}
	connected := make(chan struct{})
	remoteClosed := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		conn, err := upgrader.Upgrade(response, request, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		var update map[string]any
		if conn.ReadJSON(&update) != nil {
			return
		}
		close(connected)
		_, _, _ = conn.ReadMessage()
		close(remoteClosed)
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := connectRealtime(ctx, Config{
			OpenAIURL: strings.Replace(server.URL, "http://", "ws://", 1), OpenAIAPIKey: "key",
			Model: "gpt-realtime", Voice: "marin", Instructions: "Test.",
		})
		result <- err
	}()
	select {
	case <-connected:
	case <-time.After(time.Second):
		t.Fatal("OpenAI test connection was not established")
	}
	cancel()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("cancelled Realtime handshake returned no error")
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled Realtime handshake did not return immediately")
	}
	select {
	case <-remoteClosed:
	case <-time.After(time.Second):
		t.Fatal("OpenAI WebSocket remained open after cancellation")
	}
}

type testError struct{ message string }

func (err *testError) Error() string { return err.message }
