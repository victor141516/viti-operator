package app

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBotManagementUpdatesPipelineAndRejectsStaleReplacement(t *testing.T) {
	application, server := newIntegrationTestServer(t)
	initial := application.store.BotConfiguration()
	if initial.Revision != 1 || len(initial.ScreeningPipeline) != 0 {
		t.Fatalf("initial bot configuration = %#v", initial)
	}

	response := putBotJSON(t, server, "/api/v1/bots/pipeline", map[string]any{
		"revision": initial.Revision, "screening_pipeline": []string{"spam-filter", "voice-check"},
	})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("pipeline update status = %d", response.StatusCode)
	}
	var state BotManagementState
	if err := json.NewDecoder(response.Body).Decode(&state); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if state.Configuration.Revision != 2 || len(state.Configuration.ScreeningPipeline) != 2 {
		t.Fatalf("updated bot configuration = %#v", state.Configuration)
	}
	if got := application.screening.Pipeline(); len(got) != 2 || got[0] != "spam-filter" || got[1] != "voice-check" {
		t.Fatalf("live screening pipeline = %#v", got)
	}
	persisted, err := OpenStore(application.store.path)
	if err != nil {
		t.Fatal(err)
	}
	if got := persisted.BotConfiguration(); got.Revision != 2 || len(got.ScreeningPipeline) != 2 || got.ScreeningPipeline[1] != "voice-check" {
		t.Fatalf("persisted bot configuration = %#v", got)
	}

	response = putBotJSON(t, server, "/api/v1/bots/pipeline", map[string]any{
		"revision": initial.Revision, "screening_pipeline": []string{"spam-filter", "voice-check"},
	})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("idempotent stale pipeline update status = %d", response.StatusCode)
	}
	response.Body.Close()

	response = putBotJSON(t, server, "/api/v1/bots/pipeline", map[string]any{
		"revision": initial.Revision, "screening_pipeline": []string{"different"},
	})
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("stale pipeline update status = %d", response.StatusCode)
	}
	response.Body.Close()
	if got := application.screening.Pipeline(); len(got) != 2 || got[0] != "spam-filter" {
		t.Fatalf("stale update changed pipeline: %#v", got)
	}

	response = putBotJSON(t, server, "/api/v1/bots/pipeline", map[string]any{
		"revision": state.Configuration.Revision, "screening_pipeline": []string{"duplicate", "duplicate"},
	})
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("duplicate pipeline update status = %d", response.StatusCode)
	}
	response.Body.Close()
}

func TestAuxiliaryBotStartsInactiveAndActivationDeliversFreshSnapshot(t *testing.T) {
	application, server := newIntegrationTestServer(t)
	socket := dialIntegration(t, server, integrationHello{
		Type: "service.hello", Version: 1, Name: "realtime",
		Capabilities: map[string]json.RawMessage{"call.media": json.RawMessage(`{}`)},
	})
	defer socket.Close()
	ready := readIntegrationType(t, socket, "service.ready")
	if active, _ := ready["auxiliary_active"].(bool); active {
		t.Fatalf("new auxiliary bot was active: %#v", ready)
	}
	service := ready["service"].(map[string]any)
	connectionID := service["connection_id"].(string)
	writeIntegrationCommand(t, socket, integrationCommand{
		Type: "media.attach", Version: 1, CommandID: "inactive-media", CallID: "call-missing",
		Payload: json.RawMessage(`{"sdp_offer":"v=0\\r\\n"}`),
	})
	rejected := readCommandResult(t, socket, "inactive-media", "rejected")
	if rejected["error_code"] != "bot_inactive" {
		t.Fatalf("inactive command result = %#v", rejected)
	}

	response := putBotJSON(t, server, "/api/v1/bots/realtime/activation", map[string]any{
		"connection_id": connectionID, "enabled": true,
	})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("activation status = %d", response.StatusCode)
	}
	var state BotManagementState
	if err := json.NewDecoder(response.Body).Decode(&state); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if len(state.Bots) != 1 || !state.Bots[0].AuxiliaryActive {
		t.Fatalf("bot state after activation = %#v", state.Bots)
	}
	activation := readIntegrationType(t, socket, "service.access.updated")
	if enabled, _ := activation["auxiliary_active"].(bool); !enabled {
		t.Fatalf("activation message = %#v", activation)
	}
	snapshot := readIntegrationType(t, socket, "system.snapshot")
	payload := snapshot["payload"].(map[string]any)
	if _, found := payload["bot_configuration"]; !found {
		t.Fatalf("activation snapshot omitted bot configuration: %#v", payload)
	}
	application.events.Publish("test.after-activation", map[string]any{"value": true})
	readIntegrationType(t, socket, "test.after-activation")

	response = putBotJSON(t, server, "/api/v1/bots/realtime/activation", map[string]any{
		"connection_id": connectionID, "enabled": false,
	})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("deactivation status = %d", response.StatusCode)
	}
	response.Body.Close()
	access := readIntegrationType(t, socket, "service.access.updated")
	if enabled, _ := access["auxiliary_active"].(bool); enabled {
		t.Fatalf("deactivation message = %#v", access)
	}

	if _, _, err := application.integrations.setAuxiliaryActive("realtime", "stale-connection", false); err == nil {
		t.Fatal("stale connection ID changed bot activation")
	}
}

func TestInactiveAuxiliaryBotSuppressesDomainEventsAndCommands(t *testing.T) {
	application, server := newIntegrationTestServer(t)
	socket := dialIntegration(t, server, integrationHello{
		Type: "service.hello", Version: 1, Name: "inactive-dialer",
		Capabilities: map[string]json.RawMessage{
			"call.media": json.RawMessage(`{}`), "call.start": json.RawMessage(`{}`),
		},
	})
	defer socket.Close()
	readIntegrationType(t, socket, "service.ready")
	application.events.Publish("test.must-not-leak", map[string]any{"value": true})
	writeIntegrationCommand(t, socket, integrationCommand{
		Type: "call.start", Version: 1, CommandID: "inactive-command",
		Payload: json.RawMessage(`{"to":"+12025550123"}`),
	})
	message := readIntegrationMessage(t, socket)
	if message["type"] != "command.result" || message["error_code"] != "bot_inactive" {
		t.Fatalf("first auxiliary bot received an event or command was not gated: %#v", message)
	}
}

func putBotJSON(t *testing.T, server *httptest.Server, path string, value any) *http.Response {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPut, server.URL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}
