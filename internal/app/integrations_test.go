package app

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestIntegrationReceivesSnapshotAndBroadcastEvents(t *testing.T) {
	application, server := newIntegrationTestServer(t)
	socket := dialIntegration(t, server, integrationHello{
		Type: "service.hello", Version: 1, Name: "transcriber", Capabilities: map[string]json.RawMessage{},
	})
	defer socket.Close()

	ready := readIntegrationType(t, socket, "service.ready")
	streamID, _ := ready["stream_id"].(string)
	if streamID == "" {
		t.Fatal("service.ready omitted stream_id")
	}
	snapshot := readIntegrationType(t, socket, "system.snapshot")
	if snapshot["stream_id"] != streamID {
		t.Fatalf("snapshot stream_id = %#v, want %q", snapshot["stream_id"], streamID)
	}
	payload := snapshot["payload"].(map[string]any)
	for _, field := range []string{"calls", "messages", "known_clients", "services", "contacts", "recording", "sim_auth_assignment", "ims_registration", "runtime_status"} {
		if _, found := payload[field]; !found {
			t.Fatalf("snapshot omitted %s: %#v", field, payload)
		}
	}
	connected := readIntegrationType(t, socket, "service.connected")
	assertEventEnvelope(t, connected, streamID)

	want := application.events.Publish("test.changed", map[string]any{"value": "current"})
	event := readIntegrationType(t, socket, "test.changed")
	assertEventEnvelope(t, event, streamID)
	if got := uint64(event["sequence"].(float64)); got != want.Sequence {
		t.Fatalf("event sequence = %d, want %d", got, want.Sequence)
	}
}

func TestIntegrationRejectsDuplicateLiveNameAndAllowsReclaim(t *testing.T) {
	_, server := newIntegrationTestServer(t)
	first := dialIntegration(t, server, integrationHello{Type: "service.hello", Version: 1, Name: "screen"})
	readIntegrationType(t, first, "service.ready")

	duplicate := dialIntegration(t, server, integrationHello{Type: "service.hello", Version: 1, Name: "screen"})
	errorMessage := readIntegrationType(t, duplicate, "service.error")
	if !strings.Contains(errorMessage["message"].(string), "already connected") {
		t.Fatalf("duplicate error = %#v", errorMessage)
	}
	duplicate.Close()
	first.Close()

	deadline := time.Now().Add(2 * time.Second)
	for {
		time.Sleep(10 * time.Millisecond)
		reclaimed, _, err := websocket.DefaultDialer.Dial(webSocketURL(server.URL), nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := reclaimed.WriteJSON(integrationHello{Type: "service.hello", Version: 1, Name: "screen"}); err != nil {
			reclaimed.Close()
			t.Fatal(err)
		}
		message := readIntegrationMessage(t, reclaimed)
		if message["type"] == "service.ready" {
			reclaimed.Close()
			break
		}
		reclaimed.Close()
		if time.Now().After(deadline) {
			t.Fatal("service name was not reclaimed after disconnect")
		}
	}
}

func TestIntegrationCommandsEnforceCapabilitiesAndIdempotency(t *testing.T) {
	application, server := newIntegrationTestServer(t)
	unauthorized := dialIntegration(t, server, integrationHello{Type: "service.hello", Version: 1, Name: "observer"})
	readIntegrationType(t, unauthorized, "service.ready")
	readIntegrationType(t, unauthorized, "system.snapshot")
	readIntegrationType(t, unauthorized, "service.connected")
	writeIntegrationCommand(t, unauthorized, integrationCommand{
		Type: "call.start", Version: 1, CommandID: "denied", Payload: json.RawMessage(`{"to":"+12025550123"}`),
	})
	denied := readCommandResult(t, unauthorized, "denied", "rejected")
	if denied["error_code"] != "capability_not_granted" {
		t.Fatalf("capability rejection = %#v", denied)
	}
	unauthorized.Close()

	controller := dialIntegration(t, server, integrationHello{
		Type: "service.hello", Version: 1, Name: "controller",
		Capabilities: map[string]json.RawMessage{
			"call.start": json.RawMessage(`{}`), "call.hangup": json.RawMessage(`{}`), "call.media": json.RawMessage(`{}`),
		},
	})
	readIntegrationType(t, controller, "service.ready")
	readIntegrationType(t, controller, "system.snapshot")
	readIntegrationType(t, controller, "service.connected")
	writeIntegrationCommand(t, controller, integrationCommand{
		Type: "call.start", Version: 1, CommandID: "invalid-start", Payload: json.RawMessage(`{"to":12}`),
	})
	invalid := readNextCommandResult(t, controller, "invalid-start")
	if invalid["status"] != "rejected" || invalid["error_code"] != "invalid_payload" {
		t.Fatalf("invalid command result = %#v", invalid)
	}
	writeIntegrationCommand(t, controller, integrationCommand{Type: "call.media", Version: 1, CommandID: "unsupported-media", Payload: json.RawMessage(`{}`)})
	unsupported := readNextCommandResult(t, controller, "unsupported-media")
	if unsupported["status"] != "rejected" || unsupported["error_code"] != "unsupported_command" {
		t.Fatalf("unsupported command result = %#v", unsupported)
	}
	start := integrationCommand{
		Type: "call.start", Version: 1, CommandID: "start-1", Payload: json.RawMessage(`{"to":"+12025550123"}`),
	}
	writeIntegrationCommand(t, controller, start)
	readCommandResult(t, controller, "start-1", "accepted")
	completed := readCommandResult(t, controller, "start-1", "completed")
	call := commandResultCall(t, completed)
	if call.ID == "" || len(application.store.Calls()) != 1 {
		t.Fatalf("completed start result = %#v, calls = %#v", completed, application.store.Calls())
	}

	writeIntegrationCommand(t, controller, start)
	replayed := readCommandResult(t, controller, "start-1", "completed")
	if commandResultCall(t, replayed).ID != call.ID || len(application.store.Calls()) != 1 {
		t.Fatalf("idempotent result = %#v, calls = %#v", replayed, application.store.Calls())
	}
	start.Payload = json.RawMessage(`{"to":"+442079460018"}`)
	writeIntegrationCommand(t, controller, start)
	conflict := readCommandResult(t, controller, "start-1", "rejected")
	if conflict["error_code"] != "command_id_conflict" {
		t.Fatalf("command ID conflict = %#v", conflict)
	}

	hangup := integrationCommand{Type: "call.hangup", Version: 1, CommandID: "hangup-1", CallID: call.ID}
	writeIntegrationCommand(t, controller, hangup)
	readCommandResult(t, controller, "hangup-1", "accepted")
	readCommandResult(t, controller, "hangup-1", "completed")
	writeIntegrationCommand(t, controller, integrationCommand{Type: "call.hangup", Version: 1, CommandID: "hangup-2", CallID: call.ID})
	readCommandResult(t, controller, "hangup-2", "accepted")
	alreadyEnded := readCommandResult(t, controller, "hangup-2", "rejected")
	if alreadyEnded["error_code"] != "already_ended" {
		t.Fatalf("terminal result = %#v", alreadyEnded)
	}
	controller.Close()
}

func TestIntegrationReplaysRetainedEventsAfterResume(t *testing.T) {
	application, server := newIntegrationTestServer(t)
	streamID, _ := application.events.StreamState()
	first := application.events.Publish("before.resume", map[string]any{"ordinal": 1})
	second := application.events.Publish("after.resume", map[string]any{"ordinal": 2})

	socket := dialIntegration(t, server, integrationHello{
		Type: "service.hello", Version: 1, Name: "resumer",
		Resume: &integrationResume{StreamID: streamID, Sequence: first.Sequence},
	})
	defer socket.Close()
	ready := readIntegrationType(t, socket, "service.ready")
	if ready["latest_sequence"] != float64(second.Sequence) {
		t.Fatalf("ready = %#v, want latest sequence %d", ready, second.Sequence)
	}
	replayed := readIntegrationType(t, socket, "after.resume")
	if uint64(replayed["sequence"].(float64)) != second.Sequence {
		t.Fatalf("replayed event = %#v", replayed)
	}
	readIntegrationType(t, socket, "service.connected")
}

func TestIntegrationCommandCanRetryWhenAcceptedWasNotDelivered(t *testing.T) {
	application, _ := newIntegrationTestServer(t)
	connection := &integrationConnection{
		name: "retry-service", capabilities: map[string]json.RawMessage{"call.start": json.RawMessage(`{}`)},
	}
	command := integrationCommand{
		Type: "call.start", Version: 1, CommandID: "retry-1", Payload: json.RawMessage(`{"to":"+12025550123"}`),
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	results := make(chan integrationDelivery, 1)
	if application.handleIntegrationCommand(ctx, connection, command, results) {
		t.Fatal("command continued after accepted delivery failed")
	}
	if _, replayed, err := application.integrations.beginCommand(connection.name, command); err != nil || replayed {
		t.Fatalf("exact retry remained reserved: replayed=%v err=%v", replayed, err)
	}
}

func TestIntegrationNegotiatesServiceMediaAndRoutesICEAndDTMF(t *testing.T) {
	application, server := newIntegrationTestServer(t)
	media := &fakeServiceMediaController{}
	application.serviceMedia = media
	now := time.Now().UTC()
	call := Call{
		ID: "call-media", TraceID: "trace-media", EngineCallID: "engine-media", Instance: "engine-1",
		Direction: DirectionIncoming, Peer: "+12025550123", State: "active", StartedAt: now, UpdatedAt: now,
	}
	if err := application.store.SaveCall(call); err != nil {
		t.Fatal(err)
	}
	socket := dialIntegration(t, server, integrationHello{
		Type: "service.hello", Version: 1, Name: "transcriber",
		Capabilities: map[string]json.RawMessage{"call.media": json.RawMessage(`{}`), "call.dtmf": json.RawMessage(`{}`)},
	})
	readIntegrationType(t, socket, "service.ready")
	readIntegrationType(t, socket, "system.snapshot")
	readIntegrationType(t, socket, "service.connected")

	writeIntegrationCommand(t, socket, integrationCommand{
		Type: "media.attach", Version: 1, CommandID: "attach-1", CallID: call.ID,
		Payload: json.RawMessage(`{"sdp_offer":"v=0\\r\\n","purpose":"auxiliary"}`),
	})
	readCommandResult(t, socket, "attach-1", "accepted")
	created := readIntegrationType(t, socket, "media.session.created")
	if created["session_id"] != "media-test" || created["call_id"] != call.ID || created["sdp_answer"] != "answer" {
		t.Fatalf("media.session.created = %#v", created)
	}
	completed := readCommandResult(t, socket, "attach-1", "completed")
	payload := completed["payload"].(map[string]any)
	if payload["session_id"] != "media-test" || payload["sdp_answer"] != "answer" {
		t.Fatalf("completed attach = %#v", completed)
	}

	writeIntegrationCommand(t, socket, integrationCommand{
		Type: "media.ice", Version: 1, CommandID: "ice-1",
		Payload: json.RawMessage(`{"session_id":"media-test","candidate":"candidate:1 1 UDP 1 127.0.0.1 9 typ host"}`),
	})
	readCommandResult(t, socket, "ice-1", "accepted")
	readCommandResult(t, socket, "ice-1", "completed")
	writeIntegrationCommand(t, socket, integrationCommand{
		Type: "call.dtmf", Version: 1, CommandID: "dtmf-1", CallID: call.ID,
		Payload: json.RawMessage(`{"session_id":"media-test","digit":"#","duration_ms":300}`),
	})
	readCommandResult(t, socket, "dtmf-1", "accepted")
	readIntegrationType(t, socket, "call.dtmf")
	readCommandResult(t, socket, "dtmf-1", "completed")

	media.mu.Lock()
	if media.attach.ServiceName != "transcriber" || media.attach.Call.ID != call.ID || media.ice.SessionID != "media-test" ||
		media.dtmf.Digit != "#" || media.dtmf.DurationMS != 300 {
		t.Fatalf("media requests: attach=%#v ice=%#v dtmf=%#v", media.attach, media.ice, media.dtmf)
	}
	connectionID := media.attach.ConnectionID
	media.mu.Unlock()
	_ = socket.Close()
	deadline := time.Now().Add(2 * time.Second)
	for {
		media.mu.Lock()
		closed := media.closedConnection == connectionID
		media.mu.Unlock()
		if closed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("control disconnect did not close owned media sessions")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

type fakeServiceMediaController struct {
	mu               sync.Mutex
	attach           serviceMediaAttachRequest
	ice              serviceMediaICERequest
	dtmf             serviceMediaDTMFRequest
	closedConnection string
}

func (f *fakeServiceMediaController) Attach(_ context.Context, request serviceMediaAttachRequest) (serviceMediaDescription, error) {
	f.mu.Lock()
	f.attach = request
	f.mu.Unlock()
	return serviceMediaDescription{SessionID: "media-test", CallID: request.Call.ID, AnswerSDP: "answer", Purpose: request.Purpose}, nil
}

func (f *fakeServiceMediaController) AddICECandidate(_ context.Context, request serviceMediaICERequest) error {
	f.mu.Lock()
	f.ice = request
	f.mu.Unlock()
	return nil
}

func (f *fakeServiceMediaController) SendDTMF(_ context.Context, request serviceMediaDTMFRequest) error {
	f.mu.Lock()
	f.dtmf = request
	f.mu.Unlock()
	return nil
}

func (f *fakeServiceMediaController) CloseConnection(connectionID, _ string) {
	f.mu.Lock()
	f.closedConnection = connectionID
	f.mu.Unlock()
}

func (*fakeServiceMediaController) CloseCall(string, string) {}

func TestCallParticipantPublishesLogicalAndTransportEvents(t *testing.T) {
	application, _ := newIntegrationTestServer(t)
	streamID, sequence := application.events.StreamState()
	_, events, cancel := application.events.Subscribe(streamID, sequence)
	defer cancel()
	call := Call{ID: "call-1", TraceID: "trace-1"}
	now := time.Now().UTC()
	application.publishCallParticipantEvent("joined", call, "phone-1", "participant-1", "connection-1", now)
	application.publishCallMediaEvent("connected", call, "phone-1", "participant-1", "connection-1", "PJSIP/1", now)

	logical := <-events
	transport := <-events
	if logical.Type != "call.participant.joined" || logical.CallID != call.ID {
		t.Fatalf("logical event = %#v", logical)
	}
	if transport.Type != "call.media.connected" || transport.CallID != call.ID {
		t.Fatalf("transport event = %#v", transport)
	}
}

func newIntegrationTestServer(t *testing.T) (*App, *httptest.Server) {
	t.Helper()
	application, err := New(Config{
		DataFile: filepath.Join(t.TempDir(), "state.json"), Telephony: TelephonyConfig{Type: "memory"},
		SIMAuth: SIMAuthConfig{Type: "disabled"},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(application.Handler())
	t.Cleanup(server.Close)
	return application, server
}

func webSocketURL(serverURL string) string {
	return "ws" + strings.TrimPrefix(serverURL, "http") + "/api/v1/integrations/ws"
}

func dialIntegration(t *testing.T, server *httptest.Server, hello integrationHello) *websocket.Conn {
	t.Helper()
	socket, _, err := websocket.DefaultDialer.Dial(webSocketURL(server.URL), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := socket.WriteJSON(hello); err != nil {
		socket.Close()
		t.Fatal(err)
	}
	return socket
}

func readIntegrationMessage(t *testing.T, socket *websocket.Conn) map[string]any {
	t.Helper()
	_ = socket.SetReadDeadline(time.Now().Add(2 * time.Second))
	var message map[string]any
	if err := socket.ReadJSON(&message); err != nil {
		t.Fatal(err)
	}
	return message
}

func readIntegrationType(t *testing.T, socket *websocket.Conn, want string) map[string]any {
	t.Helper()
	for attempts := 0; attempts < 20; attempts++ {
		message := readIntegrationMessage(t, socket)
		if message["type"] == want {
			return message
		}
	}
	t.Fatalf("did not receive message type %q", want)
	return nil
}

func readCommandResult(t *testing.T, socket *websocket.Conn, commandID, status string) map[string]any {
	t.Helper()
	for attempts := 0; attempts < 30; attempts++ {
		message := readIntegrationMessage(t, socket)
		if message["type"] == "command.result" && message["command_id"] == commandID && message["status"] == status {
			return message
		}
	}
	t.Fatalf("did not receive %s result for %q", status, commandID)
	return nil
}

func readNextCommandResult(t *testing.T, socket *websocket.Conn, commandID string) map[string]any {
	t.Helper()
	for attempts := 0; attempts < 30; attempts++ {
		message := readIntegrationMessage(t, socket)
		if message["type"] == "command.result" && message["command_id"] == commandID {
			return message
		}
	}
	t.Fatalf("did not receive a result for %q", commandID)
	return nil
}

func writeIntegrationCommand(t *testing.T, socket *websocket.Conn, command integrationCommand) {
	t.Helper()
	if err := socket.WriteJSON(command); err != nil {
		t.Fatal(err)
	}
}

func commandResultCall(t *testing.T, result map[string]any) Call {
	t.Helper()
	data, err := json.Marshal(result["payload"])
	if err != nil {
		t.Fatal(err)
	}
	var call Call
	if err := json.Unmarshal(data, &call); err != nil {
		t.Fatal(err)
	}
	return call
}

func assertEventEnvelope(t *testing.T, event map[string]any, streamID string) {
	t.Helper()
	for _, field := range []string{"type", "version", "event_id", "stream_id", "sequence", "occurred_at", "payload"} {
		if _, found := event[field]; !found {
			t.Fatalf("event omitted %s: %#v", field, event)
		}
	}
	if event["stream_id"] != streamID {
		t.Fatalf("event stream_id = %#v, want %q", event["stream_id"], streamID)
	}
}
