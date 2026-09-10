package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestTopologyNodeOmitsMissingObservationTime(t *testing.T) {
	encoded, err := json.Marshal(TopologyNode{State: "failed", Label: "No authentication source"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "observed_at") || strings.Contains(string(encoded), "0001-") {
		t.Fatalf("missing evidence time must remain absent: %s", encoded)
	}
}

func TestCellularHeartbeatIsOrderedAndOmissionDoesNotEraseIt(t *testing.T) {
	a := newSIMSelectionTestApp(t, nil, "disabled")
	started := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano)
	measured := time.Now().UTC().Format(time.RFC3339Nano)
	postCellularHeartbeat(t, a, `{
		"id":"android-one","kind":"android","name":"Phone","has_sim":true,
		"cellular":{
			"generation":"process-one","generation_started_at":"`+started+`","sequence":2,
			"measured_at":"`+measured+`","subscription_role":"default_data",
			"home_network":{"availability":"available","operator":"O2","mcc":"214","mnc":"07","measured_at":"`+measured+`"},
			"serving_network":{"availability":"available","operator":"Movistar","mcc":"214","mnc":"07","measured_at":"`+measured+`"},
			"service":{"availability":"available","registration":"in_service","roaming":false,"measured_at":"`+measured+`"},
			"data_access":{"availability":"available","technology":"LTE","measured_at":"`+measured+`"},
			"signal":{"availability":"available","level":3,"dbm":-91,"measured_at":"`+measured+`"}
		}
	}`)
	first := a.topologyClients(time.Now().UTC())[0].Cellular
	if first == nil || first.Sequence != 2 || first.ReceivedAt.IsZero() || first.Stale {
		t.Fatalf("fresh cellular observation = %#v", first)
	}

	// A late request from the same process and an old client that omits the new
	// field must not replace or erase the newest complete observation.
	postCellularHeartbeat(t, a, `{
		"id":"android-one","kind":"android","name":"Phone","has_sim":true,
		"cellular":{
			"generation":"process-one","generation_started_at":"`+started+`","sequence":1,
			"measured_at":"`+measured+`",
			"subscription_role":"default_data",
			"home_network":{"availability":"unknown"},"serving_network":{"availability":"unknown"},
			"service":{"availability":"unknown"},"data_access":{"availability":"unknown"},
			"signal":{"availability":"unknown"}
		}
	}`)
	postCellularHeartbeat(t, a, `{"id":"android-one","kind":"android","name":"Phone","has_sim":true}`)
	got := a.topologyClients(time.Now().UTC())[0].Cellular
	if got == nil || got.Sequence != 2 || got.HomeNetwork.Operator != "O2" {
		t.Fatalf("ordered observation was lost: %#v", got)
	}
}

func TestCellularGenerationRejectsLateSupersededProcess(t *testing.T) {
	oldStart := time.Now().UTC().Add(-2 * time.Minute)
	newStart := oldStart.Add(-time.Hour) // The phone clock moved backwards during restart.
	old := &CellularObservation{Generation: "old", GenerationStartedAt: oldStart, Sequence: 99}
	newer := &CellularObservation{Generation: "new", GenerationStartedAt: newStart, Sequence: 1}
	lateOld := &CellularObservation{Generation: "old", GenerationStartedAt: oldStart, Sequence: 100}
	presence := clientPresence{Cellular: old}
	if !applyCellularObservation(&presence, newer) || presence.Cellular != newer {
		t.Fatal("a newly received process generation should replace the old one regardless of client wall clock")
	}
	if applyCellularObservation(&presence, lateOld) || presence.Cellular != newer {
		t.Fatal("a delayed request from a superseded process must not restore old data")
	}
}

func TestCellularObservationBecomesStaleWithoutMutatingStoredSample(t *testing.T) {
	now := time.Now().UTC()
	original := &CellularObservation{ReceivedAt: now.Add(-cellularObservationFreshWindow - time.Second)}
	snapshot := cellularSnapshot(original, now, false)
	if snapshot == nil || !snapshot.Stale {
		t.Fatalf("aged snapshot = %#v", snapshot)
	}
	if original.Stale {
		t.Fatal("staleness must be derived per response, not persisted in the observation")
	}
}

func TestCellularFieldExpiresWhileReportRemainsCurrent(t *testing.T) {
	now := time.Now().UTC()
	oldMeasurement := now.Add(-cellularMeasurementFreshWindow - time.Second)
	original := &CellularObservation{
		MeasuredAt: now, ReceivedAt: now,
		Signal: CellularSignalDetails{Availability: "available", Level: intPointer(3), DBM: intPointer(-91), MeasuredAt: &oldMeasurement},
	}
	snapshot := cellularSnapshot(original, now, false)
	if snapshot.Stale {
		t.Fatal("a current report must not be marked stale because one cached measurement expired")
	}
	if snapshot.Signal.Availability != "stale" || snapshot.Signal.Level != nil || snapshot.Signal.DBM != nil || snapshot.Signal.MeasuredAt == nil {
		t.Fatalf("expired signal evidence = %#v", snapshot.Signal)
	}
}

func TestUnavailableCellularFieldsOmitZeroMeasurementTimes(t *testing.T) {
	now := time.Now().UTC()
	encoded, err := json.Marshal(CellularObservation{
		Generation: "process-one", GenerationStartedAt: now, Sequence: 1, MeasuredAt: now, ReceivedAt: now,
		SubscriptionRole: "none",
		HomeNetwork:      CellularNetworkDetails{Availability: "unknown"}, ServingNetwork: CellularNetworkDetails{Availability: "unknown"},
		Service: CellularServiceDetails{Availability: "unknown"}, DataAccess: CellularDataAccessDetails{Availability: "unknown"},
		Signal: CellularSignalDetails{Availability: "unknown"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "0001-") || strings.Contains(string(encoded), `"measured_at":"0001`) {
		t.Fatalf("missing nested evidence timestamps must remain absent: %s", encoded)
	}
}

func TestCellularTelemetryIsVolatileAcrossManagerRestart(t *testing.T) {
	a := newSIMSelectionTestApp(t, nil, "disabled")
	started := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano)
	postCellularHeartbeat(t, a, `{
		"id":"android-one","kind":"android","name":"Phone","has_sim":true,
		"cellular":{"generation":"process-one","generation_started_at":"`+started+`","sequence":1,
		"measured_at":"`+time.Now().UTC().Format(time.RFC3339Nano)+`",
		"subscription_role":"default_data","home_network":{"availability":"unknown"},
		"serving_network":{"availability":"unknown"},"service":{"availability":"unknown"},
		"data_access":{"availability":"unknown"},"signal":{"availability":"unknown"}}
	}`)
	contents, err := os.ReadFile(a.store.path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(contents), "cellular") || strings.Contains(string(contents), "process-one") {
		t.Fatalf("volatile radio telemetry leaked into persistent state: %s", contents)
	}
}

func TestRemoteSIMExposesOnlyItsSelectedPhoneObservation(t *testing.T) {
	now := time.Now().UTC()
	observation := &CellularObservation{
		Generation: "process-one", GenerationStartedAt: now.Add(-time.Minute), Sequence: 1,
		MeasuredAt: now, ReceivedAt: now, SubscriptionRole: "default_data",
		HomeNetwork:    CellularNetworkDetails{Availability: "available", Operator: "O2", MCC: "214", MNC: "07", MeasuredAt: &now},
		ServingNetwork: CellularNetworkDetails{Availability: "available", Operator: "Movistar", MCC: "214", MNC: "07", MeasuredAt: &now},
		Service:        CellularServiceDetails{Availability: "available", Registration: "in_service", MeasuredAt: &now},
		DataAccess:     CellularDataAccessDetails{Availability: "available", Technology: "LTE", MeasuredAt: &now},
		Signal:         CellularSignalDetails{Availability: "unknown"},
	}
	a := newSIMSelectionTestApp(t, nil, "disabled")
	a.clients["android-one"] = clientPresence{ID: "android-one", Kind: "android", Name: "Phone", AuthAgentID: "agent-one", HasSIM: true, LastSeen: now, AuthLastSeen: now, Cellular: observation}
	a.simAuthAssignment = SIMAuthAssignment{ClientID: "android-one", AgentID: "agent-one", UpdatedAt: now}
	selection, clients := a.topologyClientSnapshot(now)
	sim := a.topologySIM(RuntimeStatus{}, clients, selection)
	if !sim.Selected || sim.Mode != "remote" || sim.CellularStatus != "observed" || sim.Cellular == nil || sim.Cellular.HomeNetwork.Operator != "O2" {
		t.Fatalf("remote SIM topology = %#v", sim)
	}
}

func TestOpenStickAuthenticationDoesNotClaimCellularRadioState(t *testing.T) {
	now := time.Now().UTC()
	a := newSIMSelectionTestApp(t, &readinessFixture{ready: true}, "openstick")
	sim := a.topologySIM(RuntimeStatus{SIMReady: true, UpdatedAt: now}, nil, SIMAuthSelection{Mode: "openstick"})
	if !sim.Selected || sim.CellularStatus != "not_observed" || sim.Cellular != nil {
		t.Fatalf("OpenStick radio state must remain explicitly unobserved: %#v", sim)
	}
}

func TestCellularHeartbeatRejectsSIMRoleMismatchWithoutUpdatingPresence(t *testing.T) {
	a := newSIMSelectionTestApp(t, nil, "disabled")
	now := time.Now().UTC().Format(time.RFC3339Nano)
	body := `{"id":"android-one","kind":"android","name":"Phone","has_sim":false,"cellular":{
		"generation":"process-one","generation_started_at":"` + now + `","sequence":1,"measured_at":"` + now + `",
		"subscription_role":"default_data","home_network":{"availability":"unknown"},
		"serving_network":{"availability":"unknown"},"service":{"availability":"unknown"},
		"data_access":{"availability":"unknown"},"signal":{"availability":"unknown"}}}`
	recorder := httptest.NewRecorder()
	a.clientHeartbeat(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/clients/heartbeat", strings.NewReader(body)))
	if recorder.Code != http.StatusBadRequest || len(a.clients) != 0 {
		t.Fatalf("mismatched SIM role status=%d clients=%#v", recorder.Code, a.clients)
	}
}

func TestClientKindReuseClearsIncompatibleCellularTelemetry(t *testing.T) {
	a := newSIMSelectionTestApp(t, nil, "disabled")
	now := time.Now().UTC().Format(time.RFC3339Nano)
	postCellularHeartbeat(t, a, `{"id":"shared-id","kind":"android","name":"Phone","has_sim":true,"cellular":{
		"generation":"process-one","generation_started_at":"`+now+`","sequence":1,"measured_at":"`+now+`",
		"subscription_role":"default_data","home_network":{"availability":"unknown"},
		"serving_network":{"availability":"unknown"},"service":{"availability":"unknown"},
		"data_access":{"availability":"unknown"},"signal":{"availability":"unknown"}}}`)
	postCellularHeartbeat(t, a, `{"id":"shared-id","kind":"web","name":"Browser","has_sim":false}`)
	clients := a.topologyClients(time.Now().UTC())
	if len(clients) != 1 || clients[0].Kind != "web" || clients[0].Cellular != nil {
		t.Fatalf("reused client identity retained incompatible telemetry: %#v", clients)
	}
}

func TestCellularHeartbeatRejectsInvalidSignalWithoutUpdatingPresence(t *testing.T) {
	a := newSIMSelectionTestApp(t, nil, "disabled")
	started := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano)
	body := `{"id":"android-one","kind":"android","name":"Phone","has_sim":true,"cellular":{
		"generation":"process-one","generation_started_at":"` + started + `","sequence":1,"subscription_role":"default_data",
		"home_network":{"availability":"unknown"},"serving_network":{"availability":"unknown"},
		"service":{"availability":"unknown"},"data_access":{"availability":"unknown"},
		"signal":{"availability":"available","level":5}}}`
	recorder := httptest.NewRecorder()
	a.clientHeartbeat(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/clients/heartbeat", strings.NewReader(body)))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("invalid observation status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if len(a.clients) != 0 {
		t.Fatalf("invalid telemetry partially updated presence: %#v", a.clients)
	}
}

func TestCellularHeartbeatRejectsAvailableStateWithoutEvidence(t *testing.T) {
	a := newSIMSelectionTestApp(t, nil, "disabled")
	now := time.Now().UTC().Format(time.RFC3339Nano)
	body := `{"id":"android-one","kind":"android","name":"Phone","has_sim":true,"cellular":{
		"generation":"process-one","generation_started_at":"` + now + `","sequence":1,"measured_at":"` + now + `",
		"subscription_role":"default_data","home_network":{"availability":"available","measured_at":"` + now + `"},
		"serving_network":{"availability":"unknown"},"service":{"availability":"unknown"},
		"data_access":{"availability":"unknown"},"signal":{"availability":"unknown"}}}`
	recorder := httptest.NewRecorder()
	a.clientHeartbeat(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/clients/heartbeat", strings.NewReader(body)))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("unsupported success claim status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if len(a.clients) != 0 {
		t.Fatalf("invalid evidence partially updated presence: %#v", a.clients)
	}
}

func postCellularHeartbeat(t *testing.T, a *App, body string) {
	t.Helper()
	recorder := httptest.NewRecorder()
	a.clientHeartbeat(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/clients/heartbeat", strings.NewReader(body)))
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("heartbeat status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

func intPointer(value int) *int { return &value }
