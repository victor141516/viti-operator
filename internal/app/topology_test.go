package app

import (
	"context"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTopologyViewerIsEmbeddedAndSameOriginEmbeddable(t *testing.T) {
	webRoot, err := fs.Sub(webAssets, "web")
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/topology.html", nil)
	securityHeaders(http.FileServer(http.FS(webRoot))).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("viewer status = %d", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), `id="map-viewport"`) {
		t.Fatal("embedded topology viewer is missing its interactive viewport")
	}
	if count := strings.Count(recorder.Body.String(), `/topology-inspector.bundle.js`); count != 1 {
		t.Fatalf("topology inspector bundle references = %d, want exactly one", count)
	}
	if !strings.Contains(recorder.Body.String(), "topology-viewer.js") {
		t.Fatal("topology viewer script is missing")
	}
	viewer, err := fs.ReadFile(webRoot, "topology-viewer.js")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(viewer), "An established carrier session may continue") || strings.Contains(string(viewer), "line will not work until the SIM") {
		t.Fatalf("SIM-loss guidance does not preserve honest established-session semantics: %s", viewer)
	}
	csp := recorder.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "frame-ancestors 'self'") {
		t.Fatalf("viewer cannot be framed by the web app: %q", csp)
	}
	if !strings.Contains(csp, "style-src 'self' 'unsafe-inline'") {
		t.Fatalf("viewer cannot apply calculated map coordinates: %q", csp)
	}
}

func TestClientHeartbeatIdentifiesRemoteSIMHolder(t *testing.T) {
	a := newSIMSelectionTestApp(t, nil, "disabled")
	request := httptest.NewRequest(http.MethodPost, "/api/v1/clients/heartbeat", strings.NewReader(`{
		"id":"android-123","kind":"android","name":"Google Pixel 8",
		"auth_agent_id":"pixel-agent","has_sim":true
	}`))
	recorder := httptest.NewRecorder()

	a.clientHeartbeat(recorder, request)

	if recorder.Code != http.StatusNoContent {
		t.Fatalf("heartbeat status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	claimSIMForTest(t, a, "android-123", "pixel-agent", true)
	selection, clients := a.topologyClientSnapshot(time.Now().UTC())
	if len(clients) != 1 || !clients[0].Online || !clients[0].SIMHolder {
		t.Fatalf("unexpected clients: %#v", clients)
	}
	sim := a.topologySIM(RuntimeStatus{}, clients, selection)
	if sim.State != "healthy" || sim.HolderClientID != "android-123" {
		t.Fatalf("unexpected remote SIM topology: %#v", sim)
	}
}

func TestTopologySIMUsesTheSameAtomicSelectionAsItsClientSnapshot(t *testing.T) {
	now := time.Now().UTC()
	selection := SIMAuthSelection{Mode: "remote", ClientID: "android-one", ClientName: "One", AgentID: "agent-one"}
	clients := topologyClientsFromPresence([]clientPresence{{
		ID: "android-one", Kind: "android", Name: "One", AuthAgentID: "agent-one", HasSIM: true,
		LastSeen: now, AuthLastSeen: now,
	}}, selection, now)
	a := newSIMSelectionTestApp(t, nil, "disabled")
	a.simAuthAssignment = SIMAuthAssignment{ClientID: "android-two", AgentID: "agent-two", UpdatedAt: now}

	sim := a.topologySIM(RuntimeStatus{}, clients, selection)
	if !sim.Selected || sim.HolderClientID != "android-one" || sim.AuthAgentID != "agent-one" {
		t.Fatalf("SIM node diverged from its client snapshot: %#v", sim)
	}
}

func TestLastMobileClaimWinsAndOldReleaseCannotClearIt(t *testing.T) {
	a := newSIMSelectionTestApp(t, &readinessFixture{ready: true}, "openstick")
	heartbeatClientForTest(t, a, "android-one", "agent-one", "One", true)
	heartbeatClientForTest(t, a, "android-two", "agent-two", "Two", true)
	claimSIMForTest(t, a, "android-one", "agent-one", true)
	claimSIMForTest(t, a, "android-two", "agent-two", true)
	claimSIMForTest(t, a, "android-one", "agent-one", false)

	selection := a.effectiveSIMAuthSelection(time.Now().UTC())
	if selection.Mode != "remote" || selection.ClientID != "android-two" || selection.AgentID != "agent-two" {
		t.Fatalf("last claim should remain active: %#v", selection)
	}
}

func TestOfflineClaimFallsBackToUSBAndReturnsAfterHeartbeat(t *testing.T) {
	a := newSIMSelectionTestApp(t, &readinessFixture{ready: true}, "openstick")
	heartbeatClientForTest(t, a, "android-one", "agent-one", "One", true)
	claimSIMForTest(t, a, "android-one", "agent-one", true)
	a.stateMu.Lock()
	client := a.clients["android-one"]
	client.LastSeen = time.Now().UTC().Add(-clientOnlineWindow - time.Second)
	client.AuthLastSeen = client.LastSeen
	a.clients[client.ID] = client
	a.stateMu.Unlock()

	if selection := a.effectiveSIMAuthSelection(time.Now().UTC()); selection.Mode != "openstick" {
		t.Fatalf("offline mobile should fall back to USB: %#v", selection)
	}
	heartbeatClientForTest(t, a, "android-one", "agent-one", "One", true)
	if selection := a.effectiveSIMAuthSelection(time.Now().UTC()); selection.Mode != "remote" || selection.ClientID != "android-one" {
		t.Fatalf("returning mobile should recover its assignment: %#v", selection)
	}
}

func TestRemoteAgentLossPreservesEstablishedSessionAndRecoversReadiness(t *testing.T) {
	a, err := New(Config{
		DataFile: filepath.Join(t.TempDir(), "state.json"), Telephony: TelephonyConfig{Type: "memory"},
		SIMAuth: SIMAuthConfig{Type: "remote"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	heartbeatClientForTest(t, a, "android-one", "agent-one", "One", true)
	claimSIMForTest(t, a, "android-one", "agent-one", true)
	now := time.Now().UTC()
	if err := a.store.SaveCall(Call{ID: "active-call", Direction: DirectionOutgoing, Peer: "+15555550101", State: "active", StartedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	a.stateMu.Lock()
	client := a.clients["android-one"]
	client.LastSeen = now.Add(-clientOnlineWindow - time.Second)
	client.AuthLastSeen = client.LastSeen
	a.clients[client.ID] = client
	a.stateMu.Unlock()
	status, statusErr := a.runtimeStatus(context.Background())
	if statusErr == nil || status.Ready || status.SIMReady || !status.TunnelReady || !status.IMSReady {
		t.Fatalf("remote loss status = %#v, error = %v", status, statusErr)
	}
	call, _ := a.store.GetCall("active-call")
	if call.EndedAt != nil || call.State != "active" {
		t.Fatalf("remote loss proactively ended the call: %#v", call)
	}
	heartbeatClientForTest(t, a, "android-one", "agent-one", "One", true)
	status, statusErr = a.runtimeStatus(context.Background())
	if statusErr != nil || !status.Ready || !status.SIMReady || !status.TunnelReady || !status.IMSReady {
		t.Fatalf("returning remote agent did not recover readiness: %#v, error = %v", status, statusErr)
	}
	call, _ = a.store.GetCall("active-call")
	if call.EndedAt != nil || call.State != "active" {
		t.Fatalf("remote recovery disrupted the call: %#v", call)
	}
}

func TestActiveMobileReleaseFallsBackToUSB(t *testing.T) {
	a := newSIMSelectionTestApp(t, &readinessFixture{ready: true}, "openstick")
	heartbeatClientForTest(t, a, "android-one", "agent-one", "One", true)
	claimSIMForTest(t, a, "android-one", "agent-one", true)
	claimSIMForTest(t, a, "android-one", "agent-one", false)
	if selection := a.effectiveSIMAuthSelection(time.Now().UTC()); selection.Mode != "openstick" {
		t.Fatalf("release should fall back to USB: %#v", selection)
	}
}

func TestRegularClientHeartbeatDoesNotRenewAuthenticationLease(t *testing.T) {
	a := newSIMSelectionTestApp(t, &readinessFixture{ready: true}, "openstick")
	heartbeatClientForTest(t, a, "android-one", "agent-one", "One", true)
	claimSIMForTest(t, a, "android-one", "agent-one", true)
	a.stateMu.Lock()
	client := a.clients["android-one"]
	client.AuthLastSeen = time.Now().UTC().Add(-clientOnlineWindow - time.Second)
	a.clients[client.ID] = client
	a.stateMu.Unlock()

	body := `{"id":"android-one","kind":"android","name":"One","auth_agent_id":"agent-one","has_sim":true,"auth_active":false}`
	recorder := httptest.NewRecorder()
	a.clientHeartbeat(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/clients/heartbeat", strings.NewReader(body)))
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("heartbeat status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if selection := a.effectiveSIMAuthSelection(time.Now().UTC()); selection.Mode != "openstick" {
		t.Fatalf("UI heartbeat must not keep a dead auth service selected: %#v", selection)
	}
}

func TestSIMAuthAssignmentSurvivesServerRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	want := SIMAuthAssignment{ClientID: "android-one", AgentID: "agent-one", UpdatedAt: time.Now().UTC().Truncate(time.Millisecond)}
	if err := store.SetSIMAuthAssignment(want); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.SIMAuthAssignment(); got != want {
		t.Fatalf("persisted assignment = %#v, want %#v", got, want)
	}
}

func newSIMSelectionTestApp(t *testing.T, fallback SIMAuthenticator, fallbackMode string) *App {
	t.Helper()
	store, err := OpenStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	broker := NewAuthBroker()
	a := &App{store: store, events: NewEventHub(), broker: broker, clients: make(map[string]clientPresence)}
	a.adaptiveAuth = &AdaptiveAuthenticator{Broker: broker, Fallback: fallback, FallbackMode: fallbackMode}
	a.adaptiveAuth.Select = a.effectiveSIMAuthSelection
	a.auth = a.adaptiveAuth
	return a
}

func heartbeatClientForTest(t *testing.T, a *App, clientID, agentID, name string, hasSIM bool) {
	t.Helper()
	body := `{"id":"` + clientID + `","kind":"android","name":"` + name + `","auth_agent_id":"` + agentID + `","has_sim":` + map[bool]string{true: "true", false: "false"}[hasSIM] + `,"auth_active":true}`
	recorder := httptest.NewRecorder()
	a.clientHeartbeat(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/clients/heartbeat", strings.NewReader(body)))
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("heartbeat status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

func claimSIMForTest(t *testing.T, a *App, clientID, agentID string, enabled bool) {
	t.Helper()
	body := `{"client_id":"` + clientID + `","agent_id":"` + agentID + `","enabled":` + map[bool]string{true: "true", false: "false"}[enabled] + `}`
	recorder := httptest.NewRecorder()
	a.setSIMAuthAssignment(recorder, httptest.NewRequest(http.MethodPut, "/api/v1/sim-auth/assignment", strings.NewReader(body)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("assignment status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestTopologyClientsKeepsRecentlyDisconnectedDevices(t *testing.T) {
	now := time.Now().UTC()
	a := &App{
		clients: map[string]clientPresence{
			"web-old": {ID: "web-old", Kind: "web", Name: "Chrome", LastSeen: now.Add(-clientOnlineWindow - time.Second)},
		},
	}

	clients := a.topologyClients(now)
	if len(clients) != 1 || clients[0].Online {
		t.Fatalf("recently disconnected client should remain visible as offline: %#v", clients)
	}
}

func TestTopologyClientsUsesKindSpecificVisualRetention(t *testing.T) {
	now := time.Now().UTC()
	a := &App{
		clients: map[string]clientPresence{
			"web-visible":     {ID: "web-visible", Kind: "web", Name: "Recent browser", LastSeen: now.Add(-webTopologyRetention + time.Second)},
			"web-hidden":      {ID: "web-hidden", Kind: "web", Name: "Old browser", LastSeen: now.Add(-webTopologyRetention - time.Second)},
			"android-visible": {ID: "android-visible", Kind: "android", Name: "Recent phone", LastSeen: now.Add(-androidTopologyRetention + time.Second)},
			"android-hidden":  {ID: "android-hidden", Kind: "android", Name: "Old phone", LastSeen: now.Add(-androidTopologyRetention - time.Second)},
		},
	}

	clients := a.topologyClients(now)
	if len(clients) != 2 {
		t.Fatalf("visible clients = %#v", clients)
	}
	if _, exists := a.clients["web-hidden"]; !exists {
		t.Fatal("visual expiry must not delete the browser identity")
	}
	if _, exists := a.clients["android-hidden"]; !exists {
		t.Fatal("visual expiry must not delete the Android identity")
	}
}

func TestClientHeartbeatPersistsLastSeen(t *testing.T) {
	a := newSIMSelectionTestApp(t, nil, "disabled")
	heartbeatClientForTest(t, a, "android-one", "agent-one", "One", true)

	reopened, err := OpenStore(a.store.path)
	if err != nil {
		t.Fatal(err)
	}
	clients := reopened.KnownClients()
	if len(clients) != 1 || clients[0].LastSeen.IsZero() {
		t.Fatalf("persisted clients = %#v", clients)
	}
}

func TestRememberClientThrottlesUnchangedLastSeenWrites(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	firstSeen := time.Now().UTC().Truncate(time.Millisecond)
	client := KnownClient{ID: "web-one", Kind: "web", Name: "Browser", LastSeen: firstSeen}
	if err := store.RememberClient(client); err != nil {
		t.Fatal(err)
	}
	client.LastSeen = firstSeen.Add(clientLastSeenPersistInterval - time.Second)
	if err := store.RememberClient(client); err != nil {
		t.Fatal(err)
	}
	if got := store.KnownClients()[0].LastSeen; !got.Equal(firstSeen) {
		t.Fatalf("last seen was persisted too soon: %v", got)
	}
	client.LastSeen = firstSeen.Add(clientLastSeenPersistInterval)
	if err := store.RememberClient(client); err != nil {
		t.Fatal(err)
	}
	if got := store.KnownClients()[0].LastSeen; !got.Equal(client.LastSeen) {
		t.Fatalf("last seen was not checkpointed: %v", got)
	}
}

func TestOperatorLabelUsesMobileNetworkCodes(t *testing.T) {
	got := operatorLabel("ims.mnc022.mcc214.3gppnetwork.org")
	if got != "Carrier · MCC 214 / MNC 022" {
		t.Fatalf("operatorLabel() = %q", got)
	}
}

func TestConciseTopologyDetailHidesRawAMIOutput(t *testing.T) {
	raw := "<Registration/ServerURI> volte_ims Rejected | OpenStick disconnected: device not found"
	if got := conciseTopologyDetail(raw); got != "The modem or SIM is unavailable" {
		t.Fatalf("conciseTopologyDetail() = %q", got)
	}
}
