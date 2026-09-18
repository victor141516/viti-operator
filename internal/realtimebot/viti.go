package realtimebot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

type vitiCall struct {
	ID        string                 `json:"id"`
	State     string                 `json:"state"`
	EndedAt   *time.Time             `json:"ended_at,omitempty"`
	Screening *vitiScreeningSnapshot `json:"screening,omitempty"`
}

type vitiScreeningSnapshot struct {
	State  string `json:"state,omitempty"`
	Stages []struct {
		Updates []struct {
			Action string          `json:"action"`
			Output json.RawMessage `json:"output,omitempty"`
		} `json:"updates,omitempty"`
	} `json:"stages,omitempty"`
}

type vitiScreeningCall struct {
	CallID      string `json:"call_id"`
	TraceID     string `json:"trace_id,omitempty"`
	Peer        string `json:"peer,omitempty"`
	State       string `json:"state,omitempty"`
	ContactID   string `json:"contact_id,omitempty"`
	ContactName string `json:"contact_name,omitempty"`
}

type vitiScreeningOffer struct {
	CallID        string            `json:"call_id"`
	TargetService string            `json:"target_service"`
	Context       string            `json:"context"`
	Revision      uint64            `json:"revision"`
	Call          vitiScreeningCall `json:"call"`
}

type vitiHandlers struct {
	Active              func(string)
	ActiveWhileInactive bool
	Ended               func(string)
	AccessChanged       func(bool)
	Delegated           func(callID, targetService string)
	DelegationEnded     func(callID string)
	ScreeningOffer      func(vitiScreeningOffer)
	ParticipantJoined   func(callID, serviceName string)
	ParticipantLeft     func(callID, serviceName string)
}

type vitiMessage struct {
	Type            string          `json:"type"`
	Status          string          `json:"status,omitempty"`
	CommandID       string          `json:"command_id,omitempty"`
	CommandType     string          `json:"command_type,omitempty"`
	CallID          string          `json:"call_id,omitempty"`
	ErrorCode       string          `json:"error_code,omitempty"`
	Message         string          `json:"message,omitempty"`
	AuxiliaryActive *bool           `json:"auxiliary_active,omitempty"`
	Payload         json.RawMessage `json:"payload,omitempty"`
}

type vitiCommandResult struct {
	Status    string
	ErrorCode string
	Message   string
	Payload   json.RawMessage
}

type vitiClient struct {
	conn      *websocket.Conn
	writeMu   sync.Mutex
	pendingMu sync.Mutex
	pending   map[string]chan vitiCommandResult
	counter   atomic.Uint64
}

func dialViti(ctx context.Context, config Config) (*vitiClient, error) {
	return dialVitiCapabilities(ctx, config.VitiURL, config.VitiAPIKey, config.ServiceName, map[string]any{"call.media": map[string]any{}})
}

func dialVitiCapabilities(ctx context.Context, vitiURL, apiKey, serviceName string, capabilities map[string]any) (*vitiClient, error) {
	endpoint, err := integrationURL(vitiURL)
	if err != nil {
		return nil, err
	}
	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+apiKey)
	conn, response, err := websocket.DefaultDialer.DialContext(ctx, endpoint, headers)
	if err != nil {
		if response != nil {
			return nil, fmt.Errorf("connect to Viti integration endpoint: HTTP %d: %w", response.StatusCode, err)
		}
		return nil, fmt.Errorf("connect to Viti integration endpoint: %w", err)
	}
	client := &vitiClient{conn: conn, pending: make(map[string]chan vitiCommandResult)}
	hello := map[string]any{
		"type": "service.hello", "version": 1, "name": serviceName,
		"capabilities": capabilities,
	}
	if err := client.writeJSON(hello); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("register Viti service: %w", err)
	}
	return client, nil
}

func (client *vitiClient) serve(ctx context.Context, handlers vitiHandlers) error {
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = client.conn.Close()
		case <-done:
		}
	}()
	defer close(done)
	defer client.failPending(errors.New("Viti control connection closed"))
	// Older managers did not send auxiliary_active. Default to their behavior
	// until a current service.ready or service.access.updated message arrives.
	auxiliaryActive := true
	for {
		var message vitiMessage
		if err := client.conn.ReadJSON(&message); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("read Viti integration event: %w", err)
		}
		switch message.Type {
		case "service.error":
			return errors.New(message.Message)
		case "service.ready", "service.access.updated":
			if message.AuxiliaryActive != nil {
				auxiliaryActive = *message.AuxiliaryActive
				if handlers.AccessChanged != nil {
					handlers.AccessChanged(auxiliaryActive)
				}
			}
		case "command.result":
			client.deliverResult(message)
		case "call.active":
			if !auxiliaryActive && !handlers.ActiveWhileInactive {
				continue
			}
			var payload struct {
				CallID string `json:"call_id"`
			}
			if handlers.Active != nil && json.Unmarshal(message.Payload, &payload) == nil && payload.CallID != "" {
				handlers.Active(payload.CallID)
			}
		case "call.updated":
			var call vitiCall
			if handlers.Ended != nil && json.Unmarshal(message.Payload, &call) == nil && call.ID != "" && call.EndedAt != nil {
				handlers.Ended(call.ID)
			}
		case "screening.offer":
			var offer vitiScreeningOffer
			if handlers.ScreeningOffer != nil && json.Unmarshal(message.Payload, &offer) == nil && offer.CallID != "" {
				handlers.ScreeningOffer(offer)
			}
		case "screening.delegated":
			var payload struct {
				CallID string          `json:"call_id"`
				Output json.RawMessage `json:"output"`
			}
			if handlers.Delegated != nil && json.Unmarshal(message.Payload, &payload) == nil && payload.CallID != "" {
				if target := targetService(payload.Output); target != "" {
					handlers.Delegated(payload.CallID, target)
				}
			}
		case "screening.delegation.ended":
			var payload struct {
				CallID string `json:"call_id"`
			}
			if handlers.DelegationEnded != nil && json.Unmarshal(message.Payload, &payload) == nil && payload.CallID != "" {
				handlers.DelegationEnded(payload.CallID)
			}
		case "call.participant.joined", "call.participant.left":
			var payload struct {
				CallID      string `json:"call_id"`
				ServiceName string `json:"service_name"`
			}
			if json.Unmarshal(message.Payload, &payload) == nil && payload.CallID != "" && payload.ServiceName != "" {
				if message.Type == "call.participant.joined" && handlers.ParticipantJoined != nil {
					handlers.ParticipantJoined(payload.CallID, payload.ServiceName)
				}
				if message.Type == "call.participant.left" && handlers.ParticipantLeft != nil {
					handlers.ParticipantLeft(payload.CallID, payload.ServiceName)
				}
			}
		case "system.snapshot":
			if !auxiliaryActive && !handlers.ActiveWhileInactive {
				continue
			}
			var snapshot struct {
				Calls []vitiCall `json:"calls"`
			}
			if json.Unmarshal(message.Payload, &snapshot) == nil {
				for _, call := range snapshot.Calls {
					if call.ID != "" && call.State == "active" && call.EndedAt == nil {
						if handlers.Active != nil {
							handlers.Active(call.ID)
						}
						if handlers.Delegated != nil {
							if target := snapshotDelegationTarget(call); target != "" {
								handlers.Delegated(call.ID, target)
							}
						}
					}
				}
			}
		}
	}
}

func targetService(output json.RawMessage) string {
	var payload struct {
		TargetService string `json:"target_service"`
	}
	if json.Unmarshal(output, &payload) != nil {
		return ""
	}
	return payload.TargetService
}

func snapshotDelegationTarget(call vitiCall) string {
	if call.Screening == nil || call.Screening.State != "delegated" {
		return ""
	}
	for stageIndex := len(call.Screening.Stages) - 1; stageIndex >= 0; stageIndex-- {
		updates := call.Screening.Stages[stageIndex].Updates
		for updateIndex := len(updates) - 1; updateIndex >= 0; updateIndex-- {
			if updates[updateIndex].Action == "delegate" {
				return targetService(updates[updateIndex].Output)
			}
		}
	}
	return ""
}

func (client *vitiClient) command(ctx context.Context, commandType, callID string, payload any) (json.RawMessage, error) {
	commandID := fmt.Sprintf("openai-%d-%d", time.Now().UnixMilli(), client.counter.Add(1))
	resultChannel := make(chan vitiCommandResult, 1)
	client.pendingMu.Lock()
	client.pending[commandID] = resultChannel
	client.pendingMu.Unlock()
	defer func() {
		client.pendingMu.Lock()
		delete(client.pending, commandID)
		client.pendingMu.Unlock()
	}()
	command := map[string]any{
		"type": commandType, "version": 1, "command_id": commandID,
		"call_id": callID, "payload": payload,
	}
	if err := client.writeJSON(command); err != nil {
		return nil, err
	}
	select {
	case result := <-resultChannel:
		if result.Status != "completed" {
			if result.Message == "" {
				result.Message = result.ErrorCode
			}
			return nil, fmt.Errorf("%s was %s: %s", commandType, result.Status, result.Message)
		}
		return result.Payload, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (client *vitiClient) deliverResult(message vitiMessage) {
	if message.Status == "accepted" {
		return
	}
	client.pendingMu.Lock()
	channel := client.pending[message.CommandID]
	client.pendingMu.Unlock()
	if channel == nil {
		return
	}
	select {
	case channel <- vitiCommandResult{Status: message.Status, ErrorCode: message.ErrorCode, Message: message.Message, Payload: message.Payload}:
	default:
	}
}

func (client *vitiClient) failPending(err error) {
	client.pendingMu.Lock()
	defer client.pendingMu.Unlock()
	for _, channel := range client.pending {
		select {
		case channel <- vitiCommandResult{Status: "failed", Message: err.Error()}:
		default:
		}
	}
}

func (client *vitiClient) writeJSON(value any) error {
	client.writeMu.Lock()
	defer client.writeMu.Unlock()
	_ = client.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return client.conn.WriteJSON(value)
}

func (client *vitiClient) close() {
	_ = client.conn.Close()
}
