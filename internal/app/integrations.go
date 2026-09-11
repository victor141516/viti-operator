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

var errIntegrationCommandStaleGeneration = errors.New("ephemeral command belongs to a previous service connection")

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

type integrationDelivery struct {
	value   any
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
	connection  string
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

func (r *integrationRegistry) beginCommand(service, connectionID string, command integrationCommand) (integrationCommandResult, bool, error) {
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
		if integrationCommandIsEphemeral(command.Type) && previous.connection != connectionID {
			return integrationCommandResult{}, false, errIntegrationCommandStaleGeneration
		}
		return previous.result, true, nil
	}
	r.commands[key] = &integrationCommandRecord{fingerprint: fingerprint, connection: connectionID}
	r.commandKeys = append(r.commandKeys, key)
	return integrationCommandResult{}, false, nil
}

func integrationCommandIsEphemeral(commandType string) bool {
	return commandType == "media.attach" || commandType == "media.ice"
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

func (r *integrationRegistry) abandonCommand(service string, command integrationCommand) {
	fingerprint, err := commandFingerprint(command)
	if err != nil {
		return
	}
	key := service + "\x00" + command.CommandID
	r.mu.Lock()
	record, found := r.commands[key]
	if found && !record.completed && record.fingerprint == fingerprint {
		delete(r.commands, key)
		for index, candidate := range r.commandKeys {
			if candidate == key {
				r.commandKeys = append(r.commandKeys[:index], r.commandKeys[index+1:]...)
				break
			}
		}
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
	var disconnectOnce sync.Once
	disconnect := func() {
		disconnectOnce.Do(func() {
			if a.serviceMedia != nil {
				a.serviceMedia.CloseConnection(connection.id, "control_disconnected")
			}
			if a.integrations.release(connection.name, connection.id) {
				if a.screening != nil {
					a.screening.ServiceDisconnected(connection.name, connection.id)
				}
				a.events.Publish("service.disconnected", a.integrationServicePayload(connection))
			}
		})
	}
	defer disconnect()
	defer func() {
		if a.serviceMedia != nil {
			// Commands can create resources while they are winding down. This
			// second pass is safe and closes anything that escaped cancellation.
			a.serviceMedia.CloseConnection(connection.id, "control_disconnected")
		}
	}()

	resume := integrationResume{}
	if hello.Resume != nil {
		resume = *hello.Resume
	}
	replay, events, cancel := a.events.Subscribe(resume.StreamID, resume.Sequence)
	defer cancel()
	results := make(chan integrationDelivery, integrationResultBuffer)
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
	commandCtx, cancelCommands := context.WithCancel(r.Context())
	var commands sync.WaitGroup
	for {
		var command integrationCommand
		if err := socket.ReadJSON(&command); err != nil {
			break
		}
		commands.Add(1)
		go func() {
			defer commands.Done()
			if !a.handleIntegrationCommand(commandCtx, connection, command, results) {
				_ = socket.Close()
			}
		}()
	}
	cancelCommands()
	disconnect()
	commands.Wait()
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
		if capability == "call.screen" {
			parsed := screeningCapabilityOptions{}
			if len(bytes.TrimSpace(options)) != 0 {
				if err := json.Unmarshal(options, &parsed); err != nil {
					return fmt.Errorf("invalid options for capability %q", capability)
				}
			}
			if err := parsed.validate(); err != nil {
				return fmt.Errorf("invalid options for capability %q: %w", capability, err)
			}
		}
	}
	return nil
}

func (a *App) integrationWriter(socket *websocket.Conn, connection *integrationConnection, replay EventReplay, events <-chan Event, results <-chan integrationDelivery, done <-chan struct{}) {
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
			written := writeIntegrationJSON(socket, delivery.value)
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
	contacts, contactsVersion := a.store.ContactsSnapshot("")
	statusCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	status, _ := a.runtimeStatus(statusCtx)
	cancel()
	return map[string]any{
		"type": "system.snapshot", "version": integrationProtocolVersion,
		"stream_id": replay.StreamID, "sequence": replay.LatestSequence, "occurred_at": time.Now().UTC(),
		"payload": map[string]any{
			"calls": a.store.Calls(), "messages": a.store.Messages(),
			"known_clients": a.store.KnownClients(), "services": a.integrations.snapshot(),
			"contacts":            map[string]any{"contacts": contacts, "version": contactsVersion},
			"recording":           RecordingSettings{Enabled: a.store.CallRecordingEnabled()},
			"sim_auth_assignment": a.store.SIMAuthAssignment(),
			"ims_registration":    map[string]any{"enabled": a.store.IMSRegistrationEnabled(), "registered": status.IMSReady},
			"runtime_status":      status,
		},
	}
}

func (a *App) handleIntegrationCommand(ctx context.Context, connection *integrationConnection, command integrationCommand, results chan<- integrationDelivery) bool {
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
	previous, replayed, err := a.integrations.beginCommand(connection.name, connection.id, command)
	if err != nil {
		base.Status = "rejected"
		base.ErrorCode = "command_id_conflict"
		if errors.Is(err, errIntegrationCommandStaleGeneration) {
			base.ErrorCode = "stale_connection_generation"
		}
		base.Message = err.Error()
		return queueIntegrationResult(results, base)
	}
	if replayed {
		return queueIntegrationResult(results, previous)
	}
	if !integrationCommandGranted(connection, command) {
		base.Status = "rejected"
		base.ErrorCode = "capability_not_granted"
		base.Message = "the service did not advertise the required capability"
		a.integrations.finishCommand(connection.name, base)
		return queueIntegrationResult(results, base)
	}
	if command.Type == "call.start" && a.serviceMedia != nil {
		if _, granted := connection.capabilities["call.media"]; !granted {
			base.Status = "rejected"
			base.ErrorCode = "capability_not_granted"
			base.Message = "service-originated calls also require the call.media capability"
			a.integrations.finishCommand(connection.name, base)
			return queueIntegrationResult(results, base)
		}
	}
	if code, message := validateIntegrationCommand(command); code != "" {
		base.Status = "rejected"
		base.ErrorCode = code
		base.Message = message
		a.integrations.finishCommand(connection.name, base)
		return queueIntegrationResult(results, base)
	}
	accepted := base
	accepted.Status = "accepted"
	if !writeAcceptedIntegrationResult(ctx, results, accepted) {
		a.integrations.abandonCommand(connection.name, command)
		return false
	}

	if integrationCommandAffectsCallLifecycle(command.Type) {
		a.integrations.callMu.Lock()
		defer a.integrations.callMu.Unlock()
	}
	result := a.executeIntegrationCommand(ctx, connection, command, base, results)
	a.integrations.finishCommand(connection.name, result)
	if result.Status == "completed" && command.Type == "media.attach" {
		created := map[string]any{"type": "media.session.created", "version": integrationProtocolVersion}
		if description, ok := result.Payload.(serviceMediaDescription); ok {
			created["session_id"] = description.SessionID
			created["call_id"] = description.CallID
			created["sdp_answer"] = description.AnswerSDP
			created["purpose"] = description.Purpose
		}
		if !queueIntegrationValue(results, created) {
			return false
		}
	}
	return queueIntegrationResult(results, result)
}

func integrationCommandCapability(commandType string) string {
	switch commandType {
	case "media.attach", "media.ice":
		return "call.media"
	case "screening.decision":
		return "call.screen"
	default:
		return commandType
	}
}

func integrationCommandGranted(connection *integrationConnection, command integrationCommand) bool {
	_, granted := connection.capabilities[integrationCommandCapability(command.Type)]
	if granted || command.Type != "screening.decision" {
		return granted
	}
	var payload screeningDecisionPayload
	if decodeIntegrationPayload(command.Payload, &payload) == nil && strings.EqualFold(strings.TrimSpace(payload.Action), "hangup") {
		_, granted = connection.capabilities["call.hangup"]
	}
	return granted
}

func integrationCommandAffectsCallLifecycle(commandType string) bool {
	return commandType == "call.start" || commandType == "call.hangup"
}

func validateIntegrationCommand(command integrationCommand) (string, string) {
	switch command.Type {
	case "call.start":
		payload, err := integrationStartPayload(command.Payload)
		if err != nil {
			return "invalid_payload", err.Error()
		}
		if _, err := normalizeDialDestination(payload.To, payload.Country); err != nil {
			return "invalid_payload", err.Error()
		}
	case "call.hangup":
		if integrationHangupCallID(command) == "" {
			return "invalid_payload", "call_id is required"
		}
	case "media.attach":
		if strings.TrimSpace(command.CallID) == "" {
			return "invalid_payload", "call_id is required"
		}
		var payload integrationMediaAttachPayload
		if err := decodeIntegrationPayload(command.Payload, &payload); err != nil {
			return "invalid_payload", err.Error()
		}
		if strings.TrimSpace(payload.OfferSDP) == "" {
			return "invalid_payload", "sdp_offer is required"
		}
		if payload.Purpose != "" && payload.Purpose != "auxiliary" && payload.Purpose != "controller" {
			return "invalid_payload", "purpose must be auxiliary or controller"
		}
	case "media.ice":
		var payload integrationMediaICEPayload
		if err := decodeIntegrationPayload(command.Payload, &payload); err != nil {
			return "invalid_payload", err.Error()
		}
		if strings.TrimSpace(payload.SessionID) == "" {
			return "invalid_payload", "session_id is required"
		}
	case "call.dtmf":
		if strings.TrimSpace(command.CallID) == "" {
			return "invalid_payload", "call_id is required"
		}
		var payload integrationDTMFPayload
		if err := decodeIntegrationPayload(command.Payload, &payload); err != nil {
			return "invalid_payload", err.Error()
		}
		if !strings.Contains("0123456789*#ABCD", strings.ToUpper(strings.TrimSpace(payload.Digit))) || len(strings.TrimSpace(payload.Digit)) != 1 {
			return "invalid_payload", "digit must be one of 0-9, *, #, or A-D"
		}
		if payload.Duration != 0 && (payload.Duration < 40 || payload.Duration > 6000) {
			return "invalid_payload", "duration_ms must be between 40 and 6000"
		}
	case "screening.decision":
		if strings.TrimSpace(command.CallID) == "" {
			return "invalid_payload", "call_id is required"
		}
		var payload screeningDecisionPayload
		if err := decodeIntegrationPayload(command.Payload, &payload); err != nil {
			return "invalid_payload", err.Error()
		}
		if payload.Revision == 0 {
			return "invalid_payload", "revision is required"
		}
		if !screeningDecisionAction(strings.ToLower(strings.TrimSpace(payload.Action))) {
			return "invalid_payload", "action must be take_control, next, ring_clients, or hangup"
		}
		if len(payload.Output) > screeningOutputLimit {
			return "output_too_large", "output exceeds 64 KiB"
		}
	default:
		return "unsupported_command", "this capability does not define a command in protocol version 1"
	}
	return "", ""
}

func queueIntegrationResult(results chan<- integrationDelivery, result integrationCommandResult) bool {
	return queueIntegrationValue(results, result)
}

func queueIntegrationValue(results chan<- integrationDelivery, value any) bool {
	select {
	case results <- integrationDelivery{value: value}:
		return true
	default:
		return false
	}
}

func writeAcceptedIntegrationResult(ctx context.Context, results chan<- integrationDelivery, result integrationCommandResult) bool {
	if ctx.Err() != nil {
		return false
	}
	written := make(chan bool, 1)
	timer := time.NewTimer(integrationWriteTimeout)
	defer timer.Stop()
	select {
	case results <- integrationDelivery{value: result, written: written}:
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

func writeIntegrationValue(ctx context.Context, results chan<- integrationDelivery, value any) bool {
	written := make(chan bool, 1)
	timer := time.NewTimer(integrationWriteTimeout)
	defer timer.Stop()
	select {
	case results <- integrationDelivery{value: value, written: written}:
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

func (a *App) executeIntegrationCommand(ctx context.Context, connection *integrationConnection, command integrationCommand, result integrationCommandResult, results chan<- integrationDelivery) integrationCommandResult {
	switch command.Type {
	case "call.start":
		return a.executeIntegrationStart(ctx, connection, command, result, results)
	case "call.hangup":
		return a.executeIntegrationHangup(ctx, command, result)
	case "media.attach":
		return a.executeIntegrationMediaAttach(ctx, connection, command, result)
	case "media.ice":
		return a.executeIntegrationMediaICE(ctx, connection, command, result)
	case "call.dtmf":
		return a.executeIntegrationDTMF(ctx, connection, command, result)
	case "screening.decision":
		return a.executeIntegrationScreeningDecision(connection, command, result)
	default:
		result.Status = "rejected"
		result.ErrorCode = "unsupported_command"
		result.Message = "this capability does not define a command in protocol version 1"
		return result
	}
}

func (a *App) executeIntegrationMediaAttach(ctx context.Context, connection *integrationConnection, command integrationCommand, result integrationCommandResult) integrationCommandResult {
	if a.serviceMedia == nil {
		result.Status, result.ErrorCode, result.Message = "failed", "service_media_unavailable", "service media is not enabled"
		return result
	}
	call, found := a.store.GetCall(strings.TrimSpace(command.CallID))
	if !found {
		result.Status, result.ErrorCode, result.Message = "rejected", "call_not_found", "call not found"
		return result
	}
	if call.EndedAt != nil {
		result.Status, result.ErrorCode, result.Message = "rejected", "already_ended", "call has already ended"
		return result
	}
	var payload integrationMediaAttachPayload
	_ = decodeIntegrationPayload(command.Payload, &payload)
	purpose := strings.TrimSpace(payload.Purpose)
	if purpose == "" {
		purpose = "auxiliary"
	}
	if purpose == "controller" {
		if a.screening == nil {
			result.Status, result.ErrorCode, result.Message = "rejected", "screening_not_found", "screening is not enabled"
			return result
		}
		if err := a.screening.ControllerAttachAllowed(connection.name, connection.id, call.ID); err != nil {
			result.Status, result.ErrorCode, result.Message = "rejected", screeningErrorCode(err), err.Error()
			return result
		}
	} else if call.State != "active" {
		result.Status, result.ErrorCode, result.Message = "rejected", "call_not_active", "auxiliary media can only attach after the carrier call is active"
		return result
	}
	description, err := a.serviceMedia.Attach(ctx, serviceMediaAttachRequest{
		ServiceName: connection.name, ConnectionID: connection.id, Call: call,
		OfferSDP: payload.OfferSDP, Purpose: purpose,
	})
	if err != nil {
		result.Status, result.ErrorCode, result.Message = "failed", "media_attach_failed", err.Error()
		return result
	}
	if purpose == "controller" {
		if err := a.screening.ControllerAttachAllowed(connection.name, connection.id, call.ID); err != nil {
			a.serviceMedia.CloseServiceCall(connection.name, call.ID, "screening_reservation_lost")
			result.Status, result.ErrorCode, result.Message = "rejected", screeningErrorCode(err), err.Error()
			return result
		}
	}
	result.Status, result.Payload = "completed", description
	return result
}

func (a *App) executeIntegrationScreeningDecision(connection *integrationConnection, command integrationCommand, result integrationCommandResult) integrationCommandResult {
	if a.screening == nil {
		result.Status, result.ErrorCode, result.Message = "failed", "screening_unavailable", "screening is not enabled"
		return result
	}
	var payload screeningDecisionPayload
	_ = decodeIntegrationPayload(command.Payload, &payload)
	_, canHangup := connection.capabilities["call.hangup"]
	receipt, err := a.screening.Decide(connection.name, connection.id, command.CommandID, strings.TrimSpace(command.CallID), payload, canHangup)
	if err != nil {
		result.Status, result.ErrorCode, result.Message = "rejected", screeningErrorCode(err), err.Error()
		return result
	}
	result.Status, result.Payload = "completed", receipt
	return result
}

func (a *App) executeIntegrationMediaICE(ctx context.Context, connection *integrationConnection, command integrationCommand, result integrationCommandResult) integrationCommandResult {
	if a.serviceMedia == nil {
		result.Status, result.ErrorCode, result.Message = "failed", "service_media_unavailable", "service media is not enabled"
		return result
	}
	var payload integrationMediaICEPayload
	_ = decodeIntegrationPayload(command.Payload, &payload)
	err := a.serviceMedia.AddICECandidate(ctx, serviceMediaICERequest{
		ServiceName: connection.name, ConnectionID: connection.id, SessionID: strings.TrimSpace(payload.SessionID),
		Candidate: payload.Candidate, SDPMid: payload.SDPMid, SDPMLine: payload.SDPMLine,
	})
	if err != nil {
		result.Status, result.ErrorCode, result.Message = "rejected", "media_session_invalid", err.Error()
		return result
	}
	result.Status = "completed"
	return result
}

func (a *App) executeIntegrationDTMF(ctx context.Context, connection *integrationConnection, command integrationCommand, result integrationCommandResult) integrationCommandResult {
	if a.serviceMedia == nil {
		result.Status, result.ErrorCode, result.Message = "failed", "service_media_unavailable", "service media is not enabled"
		return result
	}
	var payload integrationDTMFPayload
	_ = decodeIntegrationPayload(command.Payload, &payload)
	duration := payload.Duration
	if duration == 0 {
		duration = 250
	}
	digit := strings.ToUpper(strings.TrimSpace(payload.Digit))
	err := a.serviceMedia.SendDTMF(ctx, serviceMediaDTMFRequest{
		ServiceName: connection.name, ConnectionID: connection.id, CallID: strings.TrimSpace(command.CallID),
		SessionID: strings.TrimSpace(payload.SessionID), Digit: digit, DurationMS: duration,
	})
	if err != nil {
		result.Status, result.ErrorCode, result.Message = "rejected", "dtmf_failed", err.Error()
		return result
	}
	result.Status = "completed"
	return result
}

func (a *App) executeIntegrationStart(ctx context.Context, connection *integrationConnection, command integrationCommand, result integrationCommandResult, results chan<- integrationDelivery) integrationCommandResult {
	payload, _ := integrationStartPayload(command.Payload)
	if a.serviceMedia != nil {
		if strings.TrimSpace(payload.OfferSDP) == "" {
			result.Status, result.ErrorCode, result.Message = "rejected", "invalid_payload", "sdp_offer is required when service media is enabled"
			return result
		}
		call, status, err := a.prepareOutgoingCall(ctx, payload.To, payload.Country, command.CommandID, true)
		result.Payload = call
		if err != nil {
			result.Message = err.Error()
			if status >= http.StatusInternalServerError {
				result.Status, result.ErrorCode = "failed", "call_start_failed"
			} else {
				result.Status, result.ErrorCode = "rejected", "call_start_rejected"
			}
			return result
		}
		preparation, err := a.serviceMedia.PrepareOutgoing(ctx, serviceMediaAttachRequest{
			ServiceName: connection.name, ConnectionID: connection.id, Call: call, OfferSDP: payload.OfferSDP, Purpose: "originator",
		})
		if err != nil {
			failed, _, failure := a.failOutgoingCallStart(call, err)
			result.Payload, result.Status, result.ErrorCode, result.Message = failed, "failed", "media_prepare_failed", failure.Error()
			return result
		}
		created := map[string]any{
			"type": "media.session.created", "version": integrationProtocolVersion,
			"session_id": preparation.Description.SessionID, "call_id": preparation.Description.CallID,
			"sdp_answer": preparation.Description.AnswerSDP, "purpose": preparation.Description.Purpose,
		}
		if !writeIntegrationValue(ctx, results, created) {
			a.serviceMedia.CloseCall(call.ID, "control_delivery_failed")
			failed, _, failure := a.failOutgoingCallStart(call, errors.New("could not deliver media session answer"))
			result.Payload, result.Status, result.ErrorCode, result.Message = failed, "failed", "media_answer_delivery_failed", failure.Error()
			return result
		}
		activateCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		err = preparation.Activate(activateCtx, call.Peer)
		cancel()
		if err != nil {
			a.serviceMedia.CloseCall(call.ID, "call_start_failed")
			failed, _, failure := a.failOutgoingCallStart(call, err)
			result.Payload, result.Status, result.ErrorCode, result.Message = failed, "failed", "call_start_failed", failure.Error()
			return result
		}
		a.diag.RecordTrace("manager.call", "service.start.completed", call.TraceID, map[string]any{"call": call, "service": connection.name})
		result.Status, result.Payload = "completed", call
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

type integrationCallStartPayload struct {
	To       string `json:"to"`
	Country  string `json:"country,omitempty"`
	OfferSDP string `json:"sdp_offer,omitempty"`
}

func integrationStartPayload(raw json.RawMessage) (integrationCallStartPayload, error) {
	var payload integrationCallStartPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return integrationCallStartPayload{}, err
	}
	return payload, nil
}

func (a *App) executeIntegrationHangup(ctx context.Context, command integrationCommand, result integrationCommandResult) integrationCommandResult {
	callID := integrationHangupCallID(command)
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

func integrationHangupCallID(command integrationCommand) string {
	callID := strings.TrimSpace(command.CallID)
	if callID != "" {
		return callID
	}
	var payload struct {
		CallID string `json:"call_id"`
	}
	if err := json.Unmarshal(command.Payload, &payload); err != nil {
		return ""
	}
	return strings.TrimSpace(payload.CallID)
}
