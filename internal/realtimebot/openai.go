package realtimebot

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const realtimeReadLimit = 4 << 20

type realtimeEvent struct {
	Type         string `json:"type"`
	Delta        string `json:"delta,omitempty"`
	Text         string `json:"text,omitempty"`
	ItemID       string `json:"item_id,omitempty"`
	ContentIndex int    `json:"content_index,omitempty"`
	Error        *struct {
		Message string `json:"message"`
		Code    string `json:"code,omitempty"`
		Type    string `json:"type,omitempty"`
	} `json:"error,omitempty"`
}

type realtimeSession struct {
	conn      *websocket.Conn
	writeMu   sync.Mutex
	input     chan []byte
	closed    chan struct{}
	closeOnce sync.Once
}

func connectRealtime(ctx context.Context, config Config) (*realtimeSession, error) {
	sessionConfig := map[string]any{
		"type": "realtime", "model": config.Model, "output_modalities": []string{"audio"},
		"instructions": config.Instructions,
		"audio": map[string]any{
			"input": map[string]any{
				"format": map[string]any{"type": "audio/pcmu"},
				"turn_detection": map[string]any{
					"type": "server_vad", "create_response": true, "interrupt_response": true,
				},
			},
			"output": map[string]any{
				"format": map[string]any{"type": "audio/pcmu"}, "voice": config.Voice,
			},
		},
	}
	return connectRealtimeSession(ctx, config, sessionConfig)
}

func connectRealtimeSession(ctx context.Context, config Config, sessionConfig map[string]any) (*realtimeSession, error) {
	endpoint, err := url.Parse(config.OpenAIURL)
	if err != nil {
		return nil, err
	}
	query := endpoint.Query()
	query.Set("model", config.Model)
	endpoint.RawQuery = query.Encode()
	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+config.OpenAIAPIKey)
	conn, response, err := websocket.DefaultDialer.DialContext(ctx, endpoint.String(), headers)
	if err != nil {
		if response != nil {
			return nil, fmt.Errorf("connect to OpenAI Realtime: HTTP %d: %w", response.StatusCode, err)
		}
		return nil, fmt.Errorf("connect to OpenAI Realtime: %w", err)
	}
	session := &realtimeSession{conn: conn, input: make(chan []byte, 256), closed: make(chan struct{})}
	stopContextClose := context.AfterFunc(ctx, session.close)
	defer stopContextClose()
	conn.SetReadLimit(realtimeReadLimit)
	update := map[string]any{
		"type":    "session.update",
		"session": sessionConfig,
	}
	if err := session.writeJSON(update); err != nil {
		session.close()
		return nil, fmt.Errorf("configure OpenAI Realtime session: %w", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	for {
		var event realtimeEvent
		if err := conn.ReadJSON(&event); err != nil {
			session.close()
			return nil, fmt.Errorf("wait for OpenAI Realtime session: %w", err)
		}
		switch event.Type {
		case "session.updated":
			_ = conn.SetReadDeadline(time.Time{})
			return session, nil
		case "error":
			session.close()
			return nil, realtimeEventError(event)
		}
	}
}

func (session *realtimeSession) appendAudio(audio []byte) error {
	if len(audio) == 0 {
		return nil
	}
	copyAudio := append([]byte(nil), audio...)
	select {
	case session.input <- copyAudio:
		return nil
	case <-session.closed:
		return errors.New("OpenAI Realtime session is closed")
	default:
		return errors.New("OpenAI input audio queue is full")
	}
}

func (session *realtimeSession) run(ctx context.Context, player *pcmuPlayer) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer session.close()
	readErrors := make(chan error, 1)
	inputErrors := make(chan error, 1)
	go func() { readErrors <- session.readLoop(player) }()
	go func() { inputErrors <- session.inputLoop(runCtx) }()
	select {
	case err := <-readErrors:
		return err
	case err := <-inputErrors:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (session *realtimeSession) readLoop(player *pcmuPlayer) error {
	for {
		var event realtimeEvent
		if err := session.conn.ReadJSON(&event); err != nil {
			return fmt.Errorf("read OpenAI Realtime event: %w", err)
		}
		switch event.Type {
		case "response.output_audio.delta", "response.audio.delta":
			audio, err := base64.StdEncoding.DecodeString(event.Delta)
			if err != nil {
				return fmt.Errorf("decode OpenAI output audio: %w", err)
			}
			if err := player.append(event.ItemID, event.ContentIndex, audio); err != nil {
				return err
			}
		case "input_audio_buffer.speech_started":
			if point := player.clear(); point != nil && point.ItemID != "" {
				if err := session.writeJSON(map[string]any{
					"type": "conversation.item.truncate", "item_id": point.ItemID,
					"content_index": point.ContentIndex, "audio_end_ms": point.AudioEndMS,
				}); err != nil {
					return fmt.Errorf("truncate interrupted OpenAI output: %w", err)
				}
			}
		case "error":
			return realtimeEventError(event)
		}
	}
}

func (session *realtimeSession) inputLoop(ctx context.Context) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	buffer := make([]byte, 0, 1600)
	flush := func() error {
		if len(buffer) == 0 {
			return nil
		}
		event := map[string]any{
			"type":  "input_audio_buffer.append",
			"audio": base64.StdEncoding.EncodeToString(buffer),
		}
		buffer = buffer[:0]
		if err := session.writeJSON(event); err != nil {
			return fmt.Errorf("send audio to OpenAI Realtime: %w", err)
		}
		return nil
	}
	for {
		select {
		case audio := <-session.input:
			buffer = append(buffer, audio...)
			if len(buffer) >= 3200 {
				if err := flush(); err != nil {
					return err
				}
			}
		case <-ticker.C:
			if err := flush(); err != nil {
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		case <-session.closed:
			return errors.New("OpenAI Realtime session is closed")
		}
	}
}

func realtimeEventError(event realtimeEvent) error {
	if event.Error == nil {
		return errors.New("OpenAI Realtime returned an unspecified error")
	}
	if event.Error.Code != "" {
		return fmt.Errorf("OpenAI Realtime error %s: %s", event.Error.Code, event.Error.Message)
	}
	return fmt.Errorf("OpenAI Realtime error: %s", event.Error.Message)
}

func (session *realtimeSession) writeJSON(value any) error {
	session.writeMu.Lock()
	defer session.writeMu.Unlock()
	_ = session.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return session.conn.WriteJSON(value)
}

func (session *realtimeSession) close() {
	session.closeOnce.Do(func() {
		close(session.closed)
		_ = session.conn.WriteControl(
			websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, "session ended"),
			time.Now().Add(time.Second),
		)
		_ = session.conn.Close()
	})
}
