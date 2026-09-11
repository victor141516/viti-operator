package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	integrationProtocolVersion = 1
	integrationReadLimit       = 128 << 10
	integrationWriteTimeout    = 10 * time.Second
	integrationPongTimeout     = 75 * time.Second
	integrationPingInterval    = 30 * time.Second
	integrationResultBuffer    = 16
	integrationCommandLimit    = 1024
)

var integrationNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

var supportedIntegrationCapabilities = map[string]struct{}{
	"call.screen": {},
	"call.media":  {},
	"call.hangup": {},
	"call.dtmf":   {},
	"call.start":  {},
}

type integrationHello struct {
	Type         string                     `json:"type"`
	Version      int                        `json:"version"`
	Name         string                     `json:"name"`
	Capabilities map[string]json.RawMessage `json:"capabilities"`
	Resume       *integrationResume         `json:"resume,omitempty"`
}

type integrationResume struct {
	StreamID string `json:"stream_id"`
	Sequence uint64 `json:"sequence"`
}

type integrationCommand struct {
	Type      string          `json:"type"`
	Version   int             `json:"version"`
	CommandID string          `json:"command_id"`
	CallID    string          `json:"call_id,omitempty"`
	Payload   json.RawMessage `json:"payload,omitempty"`
}

type integrationCommandResult struct {
	Type        string `json:"type"`
	Version     int    `json:"version"`
	CommandID   string `json:"command_id"`
	CommandType string `json:"command_type"`
	Status      string `json:"status"`
	ErrorCode   string `json:"error_code,omitempty"`
	Message     string `json:"message,omitempty"`
	Payload     any    `json:"payload,omitempty"`
}

type integrationResultDelivery struct {
	result  integrationCommandResult
	written chan bool
}

type integrationConnection struct {
	id           string
	name         string
	capabilities map[string]json.RawMessage
	connectedAt  time.Time
}

type integrationCommandRecord struct {
	fingerprint [sha256.Size]byte
	result      integrationCommandResult
	completed   bool
}

type integrationRegistry struct {
	mu          sync.Mutex
	connections map[string]*integrationConnection
	commands    map[string]*integrationCommandRecord
	commandKeys []string
	callMu      sync.Mutex
}

func newIntegrationRegistry() *integrationRegistry {
	return &integrationRegistry{
		connections: make(map[string]*integrationConnection),
		commands:    make(map[string]*integrationCommandRecord),
	}
}

func (r *integrationRegistry) claim(connection *integrationConnection) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, found := r.connections[connection.name]; found {
		return false
	}
	r.connections[connection.name] = connection
	return true
}

func (r *integrationRegistry) release(name, id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	connection, found := r.connections[name]
	if !found || connection.id != id {
		return false
	}
	delete(r.connections, name)
	return true
}

func (r *integrationRegistry) snapshot() []map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	services := make([]map[string]any, 0, len(r.connections))
	for _, connection := range r.connections {
		services = append(services, map[string]any{
			"name": connection.name, "connection_id": connection.id,
			"capabilities": connection.capabilities, "connected_at": connection.connectedAt,
		})
	}
	sort.Slice(services, func(i, j int) bool { return services[i]["name"].(string) < services[j]["name"].(string) })
	return services
}

func (r *integrationRegistry) beginCommand(service string, command integrationCommand) (integrationCommandResult, bool, error) {
	fingerprint, err := commandFingerprint(command)
	if err != nil {
		return integrationCommandResult{}, false, err
	}
	key := service + "\x00" + command.CommandID
	r.mu.Lock()
	defer r.mu.Unlock()
	if previous, found := r.commands[key]; found {
		if previous.fingerprint != fingerprint {
			return integrationCommandResult{}, false, errors.New("command_id was already used with different content")
		}
		if !previous.completed {
			return integrationCommandResult{}, false, errors.New("command is already in progress")
		}
		return previous.result, true, nil
	}
	r.commands[key] = &integrationCommandRecord{fingerprint: fingerprint}
	r.commandKeys = append(r.commandKeys, key)
	return integrationCommandResult{}, false, nil
}

func (r *integrationRegistry) finishCommand(service string, result integrationCommandResult) {
	key := service + "\x00" + result.CommandID
	r.mu.Lock()
	if record, found := r.commands[key]; found {
		record.result = result
		record.completed = true
	}
	for len(r.commandKeys) > integrationCommandLimit {
		oldest := r.commandKeys[0]
		record := r.commands[oldest]
		if record != nil && !record.completed {
			break
		}
		delete(r.commands, oldest)
		r.commandKeys = r.commandKeys[1:]
	}
	r.mu.Unlock()
}

func commandFingerprint(command integrationCommand) ([sha256.Size]byte, error) {
	canonical, err := json.Marshal(command)
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	var value any
	if err := json.Unmarshal(canonical, &value); err != nil {
		return [sha256.Size]byte{}, err
	}
	canonical, err = json.Marshal(value)
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	return sha256.Sum256(canonical), nil
}

var integrationUpgrader = websocket.Upgrader{
	HandshakeTimeout: 10 * time.Second,
	CheckOrigin: func(r *http.Request) bool {
		origin := strings.TrimSpace(r.Header.Get("Origin"))
		return origin == "" || sameWebSocketOrigin(origin, r.Host)
	},
}

func sameWebSocketOrigin(origin, host string) bool {
	return origin == "http://"+host || origin == "https://"+host
}

func (a *App) integrationWebSocket(w http.ResponseWriter, r *http.Request) {
	socket, err := integrationUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer socket.Close()
	socket.SetReadLimit(integrationReadLimit)
	_ = socket.SetReadDeadline(time.Now().Add(15 * time.Second))

	var hello integrationHello
	if err := socket.ReadJSON(&hello); err != nil {
		a.writeIntegrationClose(socket, websocket.ClosePolicyViolation, "service.hello is required")
		return
	}
	if err := validateIntegrationHello(hello); err != nil {
		_ = socket.WriteJSON(map[string]any{"type": "service.error", "version": integrationProtocolVersion, "message": err.Error()})
		a.writeIntegrationClose(socket, websocket.ClosePolicyViolation, err.Error())
		return
	}
	connectionID, err := randomID("integration")
	if err != nil {
		a.writeIntegrationClose(socket, websocket.CloseInternalServerErr, "could not allocate connection")
		return
	}
	connection := &integrationConnection{
		id: connectionID, name: hello.Name, capabilities: hello.Capabilities, connectedAt: time.Now().UTC(),
	}
	if !a.integrations.claim(connection) {
		_ = socket.WriteJSON(map[string]any{"type": "service.error", "version": integrationProtocolVersion, "message": "service name is already connected"})
		a.writeIntegrationClose(socket, websocket.ClosePolicyViolation, "duplicate service name")
		return
	}
	defer func() {
		if a.integrations.release(connection.name, connection.id) {
			a.events.Publish("service.disconnected", a.integrationServicePayload(connection))
		}
	}()

	resume := integrationResume{}
	if hello.Resume != nil {
		resume = *hello.Resume
	}
	replay, events, cancel := a.events.Subscribe(resume.StreamID, resume.Sequence)
	defer cancel()
	results := make(chan integrationResultDelivery, integrationResultBuffer)
	done := make(chan struct{})
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		a.integrationWriter(socket, connection, replay, events, results, done)
	}()

	a.events.Publish("service.connected", a.integrationServicePayload(connection))
	_ = socket.SetReadDeadline(time.Now().Add(integrationPongTimeout))
	socket.SetPongHandler(func(string) error {
		return socket.SetReadDeadline(time.Now().Add(integrationPongTimeout))
	})
	for {
		var command integrationCommand
		if err := socket.ReadJSON(&command); err != nil {
			break
		}
		if !a.handleIntegrationCommand(r.Context(), connection, command, results) {
			break
		}
	}
	close(done)
	<-writerDone
}

func validateIntegrationHello(hello integrationHello) error {
	if hello.Type != "service.hello" {
		return errors.New("first message must be service.hello")
	}
	if hello.Version != integrationProtocolVersion {
		return fmt.Errorf("unsupported protocol version %d", hello.Version)
	}
	if !integrationNamePattern.MatchString(hello.Name) {
		return errors.New("name must contain 1-64 letters, digits, dots, underscores, or hyphens")
	}
	for capability, options := range hello.Capabilities {
		if _, supported := supportedIntegrationCapabilities[capability]; !supported {
			return fmt.Errorf("unsupported capability %q", capability)
		}
		if len(bytes.TrimSpace(options)) > 0 && !json.Valid(options) {
			return fmt.Errorf("invalid options for capability %q", capability)
		}
	}
	return nil
}

func (a *App) integrationWriter(socket *websocket.Conn, connection *integrationConnection, replay EventReplay, events <-chan Event, results <-chan integrationResultDelivery, done <-chan struct{}) {
	defer socket.Close()
	ready := map[string]any{
		"type": "service.ready", "version": integrationProtocolVersion,
		"service": a.integrationServicePayload(connection), "stream_id": replay.StreamID,
		"latest_sequence": replay.LatestSequence,
	}
	if !writeIntegrationJSON(socket, ready) {
		return
	}
	if replay.SnapshotRequired {
		if !writeIntegrationJSON(socket, a.integrationSnapshot(replay)) {
			return
		}
	} else {
		for _, event := range replay.Events {
			if !writeIntegrationJSON(socket, event) {
				return
			}
		}
	}
	ticker := time.NewTicker(integrationPingInterval)
	defer ticker.Stop()
	for {
		select {
		case event, open := <-events:
			if !open {
				a.writeIntegrationClose(socket, websocket.ClosePolicyViolation, "outbound event queue exceeded")
				return
			}
			if !writeIntegrationJSON(socket, event) {
				return
			}
		case delivery := <-results:
			written := writeIntegrationJSON(socket, delivery.result)
			if delivery.written != nil {
				delivery.written <- written
				close(delivery.written)
			}
			if !written {
				return
			}
		case <-ticker.C:
			_ = socket.SetWriteDeadline(time.Now().Add(integrationWriteTimeout))
			if err := socket.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		case <-done:
			return
		}
	}
}

func writeIntegrationJSON(socket *websocket.Conn, value any) bool {
	_ = socket.SetWriteDeadline(time.Now().Add(integrationWriteTimeout))
	return socket.WriteJSON(value) == nil
}

func (a *App) writeIntegrationClose(socket *websocket.Conn, code int, reason string) {
	_ = socket.SetWriteDeadline(time.Now().Add(integrationWriteTimeout))
	_ = socket.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, reason), time.Now().Add(integrationWriteTimeout))
}

func (a *App) integrationServicePayload(connection *integrationConnection) map[string]any {
	return map[string]any{
		"name": connection.name, "connection_id": connection.id,
		"capabilities": connection.capabilities, "connected_at": connection.connectedAt,
	}
}

func (a *App) integrationSnapshot(replay EventReplay) map[string]any {
	return map[string]any{
		"type": "system.snapshot", "version": integrationProtocolVersion,
		"stream_id": replay.StreamID, "sequence": replay.LatestSequence, "occurred_at": time.Now().UTC(),
		"payload": map[string]any{
			"calls": a.store.Calls(), "messages": a.store.Messages(),
			"known_clients": a.store.KnownClients(), "services": a.integrations.snapshot(),
		},
	}
}

func (a *App) handleIntegrationCommand(ctx context.Context, connection *integrationConnection, command integrationCommand, results chan<- integrationResultDelivery) bool {
	base := integrationCommandResult{
		Type: "command.result", Version: integrationProtocolVersion,
		CommandID: command.CommandID, CommandType: command.Type,
	}
	if command.Version != integrationProtocolVersion || strings.TrimSpace(command.CommandID) == "" {
		base.Status = "rejected"
		base.ErrorCode = "invalid_command"
		base.Message = "version 1 and a non-empty command_id are required"
		return queueIntegrationResult(results, base)
	}
	previous, replayed, err := a.integrations.beginCommand(connection.name, command)
	if err != nil {
		base.Status = "rejected"
		base.ErrorCode = "command_id_conflict"
		base.Message = err.Error()
		return queueIntegrationResult(results, base)
	}
	if replayed {
		return queueIntegrationResult(results, previous)
	}
	if _, granted := connection.capabilities[command.Type]; !granted {
		base.Status = "rejected"
		base.ErrorCode = "capability_not_granted"
		base.Message = "the service did not advertise the required capability"
		a.integrations.finishCommand(connection.name, base)
		return queueIntegrationResult(results, base)
	}
	accepted := base
	accepted.Status = "accepted"
	if !writeAcceptedIntegrationResult(ctx, results, accepted) {
		return false
	}

	a.integrations.callMu.Lock()
	result := a.executeIntegrationCommand(ctx, command, base)
	a.integrations.callMu.Unlock()
	a.integrations.finishCommand(connection.name, result)
	return queueIntegrationResult(results, result)
}

func queueIntegrationResult(results chan<- integrationResultDelivery, result integrationCommandResult) bool {
	select {
	case results <- integrationResultDelivery{result: result}:
		return true
	default:
		return false
	}
}

func writeAcceptedIntegrationResult(ctx context.Context, results chan<- integrationResultDelivery, result integrationCommandResult) bool {
	written := make(chan bool, 1)
	timer := time.NewTimer(integrationWriteTimeout)
	defer timer.Stop()
	select {
	case results <- integrationResultDelivery{result: result, written: written}:
	case <-ctx.Done():
		return false
	case <-timer.C:
		return false
	}
	select {
	case ok := <-written:
		return ok
	case <-ctx.Done():
		return false
	case <-timer.C:
		return false
	}
}

func (a *App) executeIntegrationCommand(ctx context.Context, command integrationCommand, result integrationCommandResult) integrationCommandResult {
	switch command.Type {
	case "call.start":
		return a.executeIntegrationStart(ctx, command, result)
	case "call.hangup":
		return a.executeIntegrationHangup(ctx, command, result)
	default:
		result.Status = "rejected"
		result.ErrorCode = "unsupported_command"
		result.Message = "this capability does not define a command in protocol version 1"
		return result
	}
}

func (a *App) executeIntegrationStart(ctx context.Context, command integrationCommand, result integrationCommandResult) integrationCommandResult {
	var payload struct {
		To      string `json:"to"`
		Country string `json:"country,omitempty"`
	}
	if err := json.Unmarshal(command.Payload, &payload); err != nil {
		result.Status, result.ErrorCode, result.Message = "rejected", "invalid_payload", err.Error()
		return result
	}
	call, status, err := a.startCallOperation(ctx, payload.To, payload.Country, command.CommandID)
	result.Payload = call
	if err == nil {
		result.Status = "completed"
		return result
	}
	result.Message = err.Error()
	if status >= http.StatusInternalServerError {
		result.Status, result.ErrorCode = "failed", "call_start_failed"
	} else {
		result.Status, result.ErrorCode = "rejected", "call_start_rejected"
	}
	return result
}

func (a *App) executeIntegrationHangup(ctx context.Context, command integrationCommand, result integrationCommandResult) integrationCommandResult {
	callID := strings.TrimSpace(command.CallID)
	if callID == "" {
		var payload struct {
			CallID string `json:"call_id"`
		}
		_ = json.Unmarshal(command.Payload, &payload)
		callID = strings.TrimSpace(payload.CallID)
	}
	call, found := a.store.GetCall(callID)
	if !found {
		result.Status, result.ErrorCode, result.Message = "rejected", "call_not_found", "call not found"
		return result
	}
	if call.EndedAt != nil {
		result.Status, result.ErrorCode, result.Message = "rejected", "already_ended", "call has already ended"
		result.Payload = call
		return result
	}
	call, status, err := a.hangupCallOperation(ctx, callID)
	result.Payload = call
	if err == nil {
		result.Status = "completed"
		return result
	}
	result.Message = err.Error()
	if status >= http.StatusInternalServerError {
		result.Status, result.ErrorCode = "failed", "call_hangup_failed"
	} else {
		result.Status, result.ErrorCode = "rejected", "call_hangup_rejected"
	}
	return result
}
