package app

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	clientOnlineWindow       = 40 * time.Second
	webTopologyRetention     = 10 * time.Minute
	androidTopologyRetention = 24 * time.Hour
)

var topologyIdentifierPattern = regexp.MustCompile(`^[a-zA-Z0-9._:-]+$`)
var mobileRealmPattern = regexp.MustCompile(`(?i)mnc([0-9]{3})\.mcc([0-9]{3})`)

type clientPresence struct {
	ID           string
	Kind         string
	Name         string
	AuthAgentID  string
	HasSIM       bool
	LastSeen     time.Time
	AuthLastSeen time.Time
	Cellular     *CellularObservation
}

type KnownClient struct {
	ID          string    `json:"id"`
	Kind        string    `json:"kind"`
	Name        string    `json:"name"`
	AuthAgentID string    `json:"auth_agent_id,omitempty"`
	HasSIM      bool      `json:"has_sim"`
	LastSeen    time.Time `json:"last_seen,omitempty"`
}

type TopologyClient struct {
	ID          string               `json:"id"`
	Kind        string               `json:"kind"`
	Name        string               `json:"name"`
	AuthAgentID string               `json:"auth_agent_id,omitempty"`
	HasSIM      bool                 `json:"has_sim"`
	SIMHolder   bool                 `json:"sim_holder"`
	Online      bool                 `json:"online"`
	LastSeen    time.Time            `json:"last_seen"`
	Cellular    *CellularObservation `json:"cellular,omitempty"`
}

type TopologyNode struct {
	State      string     `json:"state"`
	Label      string     `json:"label"`
	Detail     string     `json:"detail,omitempty"`
	Evidence   string     `json:"evidence,omitempty"`
	ObservedAt *time.Time `json:"observed_at,omitempty"`
	MCC        string     `json:"mcc,omitempty"`
	MNC        string     `json:"mnc,omitempty"`
}

type TopologySIM struct {
	TopologyNode
	Mode           string               `json:"mode"`
	HolderClientID string               `json:"holder_client_id,omitempty"`
	AuthAgentID    string               `json:"auth_agent_id,omitempty"`
	Selected       bool                 `json:"selected"`
	CellularStatus string               `json:"cellular_status"`
	Cellular       *CellularObservation `json:"cellular,omitempty"`
}

type TopologyResponse struct {
	GeneratedAt time.Time        `json:"generated_at"`
	Server      TopologyNode     `json:"server"`
	SIM         TopologySIM      `json:"sim"`
	EPDG        TopologyNode     `json:"epdg"`
	IMS         TopologyNode     `json:"ims"`
	Operator    TopologyNode     `json:"operator"`
	Clients     []TopologyClient `json:"clients"`
}

type SIMAuthAssignmentState struct {
	Mode              string    `json:"mode"`
	ClientID          string    `json:"client_id,omitempty"`
	ClientName        string    `json:"client_name,omitempty"`
	AgentID           string    `json:"agent_id,omitempty"`
	ClaimedClientID   string    `json:"claimed_client_id,omitempty"`
	ClaimedAgentID    string    `json:"claimed_agent_id,omitempty"`
	UpdatedAt         time.Time `json:"updated_at,omitempty"`
	FallbackAvailable bool      `json:"fallback_available"`
}

func (a *App) clientHeartbeat(w http.ResponseWriter, r *http.Request) {
	var request struct {
		ID          string          `json:"id"`
		Kind        string          `json:"kind"`
		Name        string          `json:"name"`
		AuthAgentID string          `json:"auth_agent_id"`
		HasSIM      bool            `json:"has_sim"`
		AuthActive  bool            `json:"auth_active"`
		Cellular    json.RawMessage `json:"cellular"`
	}
	if err := readJSON(r, &request, 8<<10); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	request.ID = strings.TrimSpace(request.ID)
	request.Kind = strings.ToLower(strings.TrimSpace(request.Kind))
	request.Name = strings.TrimSpace(request.Name)
	request.AuthAgentID = strings.TrimSpace(request.AuthAgentID)
	if len(request.ID) < 3 || len(request.ID) > 128 || !topologyIdentifierPattern.MatchString(request.ID) {
		writeError(w, http.StatusBadRequest, "invalid client id")
		return
	}
	if request.Kind != "web" && request.Kind != "android" {
		writeError(w, http.StatusBadRequest, "client kind must be web or android")
		return
	}
	if request.Name == "" || len(request.Name) > 80 || len(request.AuthAgentID) > 128 {
		writeError(w, http.StatusBadRequest, "invalid client name or agent id")
		return
	}

	now := time.Now().UTC()
	cellular, cellularPresent, err := decodeCellularHeartbeat(request.Cellular, now)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if cellularPresent && request.Kind != "android" {
		writeError(w, http.StatusBadRequest, "only Android clients can report cellular observations")
		return
	}
	a.stateMu.Lock()
	previous := a.clients[request.ID]
	authLastSeen := previous.AuthLastSeen
	if request.AuthActive {
		authLastSeen = now
	} else if !request.HasSIM {
		authLastSeen = time.Time{}
	}
	if cellularPresent && shouldReplaceCellular(previous.Cellular, cellular) {
		previous.Cellular = cellular
	}
	a.clients[request.ID] = clientPresence{
		ID: request.ID, Kind: request.Kind, Name: request.Name,
		AuthAgentID: request.AuthAgentID, HasSIM: request.HasSIM, LastSeen: now, AuthLastSeen: authLastSeen,
		Cellular: previous.Cellular,
	}
	a.stateMu.Unlock()
	if a.store != nil {
		if err := a.store.RememberClient(KnownClient{
			ID: request.ID, Kind: request.Kind, Name: request.Name,
			AuthAgentID: request.AuthAgentID, HasSIM: request.HasSIM, LastSeen: now,
		}); err != nil && a.logger != nil {
			a.logger.Warn("could not remember topology client", "client_id", request.ID, "error", err)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) simAuthAssignmentState(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, a.currentSIMAuthAssignmentState(time.Now().UTC()))
}

func (a *App) setSIMAuthAssignment(w http.ResponseWriter, r *http.Request) {
	var request struct {
		ClientID string `json:"client_id"`
		AgentID  string `json:"agent_id"`
		Enabled  *bool  `json:"enabled"`
	}
	if err := readJSON(r, &request, 4<<10); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	request.ClientID = strings.TrimSpace(request.ClientID)
	request.AgentID = strings.TrimSpace(request.AgentID)
	if request.Enabled == nil || !topologyIdentifierPattern.MatchString(request.ClientID) || !topologyIdentifierPattern.MatchString(request.AgentID) {
		writeError(w, http.StatusBadRequest, "client_id, agent_id and enabled are required")
		return
	}

	a.simAuthMu.Lock()
	defer a.simAuthMu.Unlock()
	now := time.Now().UTC()
	a.stateMu.RLock()
	previous := a.simAuthAssignment
	client, known := a.clients[request.ClientID]
	a.stateMu.RUnlock()
	next := previous
	if *request.Enabled {
		if !known || client.Kind != "android" || !client.HasSIM || client.LastSeen.IsZero() || now.Sub(client.LastSeen) > clientOnlineWindow {
			writeError(w, http.StatusConflict, "the phone must be connected and have an available SIM")
			return
		}
		if client.AuthAgentID != request.AgentID {
			writeError(w, http.StatusConflict, "the agent does not match the identity advertised by the phone")
			return
		}
		next = SIMAuthAssignment{ClientID: request.ClientID, AgentID: request.AgentID, UpdatedAt: now}
		a.stateMu.Lock()
		client.AuthLastSeen = now
		a.clients[request.ClientID] = client
		a.stateMu.Unlock()
	} else if previous.ClientID == request.ClientID {
		next = SIMAuthAssignment{}
	}
	if next != previous {
		if err := a.store.SetSIMAuthAssignment(next); err != nil {
			writeError(w, http.StatusInternalServerError, "could not save the authentication assignment")
			return
		}
		a.stateMu.Lock()
		a.simAuthAssignment = next
		a.stateMu.Unlock()
		a.diag.RecordEvent("manager.sim-auth", "assignment.changed", map[string]any{"previous": previous, "current": next})
		a.events.Publish("sim-auth.assignment.updated", next)
	}
	writeJSON(w, http.StatusOK, a.currentSIMAuthAssignmentState(now))
}

func (a *App) effectiveSIMAuthSelection(now time.Time) SIMAuthSelection {
	a.stateMu.RLock()
	assignment := a.simAuthAssignment
	client, known := a.clients[assignment.ClientID]
	a.stateMu.RUnlock()
	if known && assignment.ClientID != "" && assignment.AgentID != "" && client.Kind == "android" && client.HasSIM &&
		client.AuthAgentID == assignment.AgentID && !client.AuthLastSeen.IsZero() && now.Sub(client.AuthLastSeen) <= clientOnlineWindow {
		return SIMAuthSelection{Mode: "remote", ClientID: client.ID, ClientName: client.Name, AgentID: assignment.AgentID}
	}
	if a.adaptiveAuth != nil && a.adaptiveAuth.Fallback != nil {
		return SIMAuthSelection{Mode: a.adaptiveAuth.FallbackMode}
	}
	return SIMAuthSelection{Mode: "disabled"}
}

func (a *App) currentSIMAuthAssignmentState(now time.Time) SIMAuthAssignmentState {
	selection := a.effectiveSIMAuthSelection(now)
	a.stateMu.RLock()
	assignment := a.simAuthAssignment
	a.stateMu.RUnlock()
	return SIMAuthAssignmentState{
		Mode: selection.Mode, ClientID: selection.ClientID, ClientName: selection.ClientName,
		AgentID: selection.AgentID, ClaimedClientID: assignment.ClientID, ClaimedAgentID: assignment.AgentID,
		UpdatedAt:         assignment.UpdatedAt,
		FallbackAvailable: a.adaptiveAuth != nil && a.adaptiveAuth.Fallback != nil,
	}
}

func (a *App) topology(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	status, _ := a.runtimeStatus(ctx)
	now := time.Now().UTC()
	clients := a.topologyClients(now)
	registrationEnabled := a.store.IMSRegistrationEnabled()

	response := TopologyResponse{
		GeneratedAt: now,
		Server:      TopologyNode{State: "healthy", Label: "Viti server", Detail: "API available", Evidence: "Manager API", ObservedAt: observedTime(now)},
		EPDG:        topologyRuntimeNode("ePDG tunnel", status.TunnelReady, true, status.Detail),
		IMS:         topologyRuntimeNode("IMS registration", status.IMSReady, registrationEnabled, status.Detail),
		Clients:     clients,
	}
	response.SIM = a.topologySIM(status, clients, now)
	response.IMS.Evidence, response.IMS.ObservedAt = runtimeEvidence(status.Provider), observedTime(status.UpdatedAt)
	a.stateMu.RLock()
	tunnel := a.tunnel
	a.stateMu.RUnlock()
	if tunnel.Seen {
		response.EPDG.Evidence, response.EPDG.ObservedAt = "Engine tunnel event", observedTime(tunnel.UpdatedAt)
	} else {
		response.EPDG.Evidence, response.EPDG.ObservedAt = "Telephony runtime", observedTime(status.UpdatedAt)
	}
	mcc, mnc := mobileNetworkCodes(a.cfg.Telephony.AMI.Realm)
	response.Operator = TopologyNode{
		State: response.IMS.State, Label: operatorLabel(a.cfg.Telephony.AMI.Realm),
		Evidence:   "Carrier identity from configured IMS realm; registration from " + runtimeEvidence(status.Provider),
		ObservedAt: observedTime(status.UpdatedAt), MCC: mcc, MNC: mnc,
	}
	switch response.Operator.State {
	case "healthy":
		response.Operator.Detail = "Line registered"
	case "idle":
		response.Operator.Detail = "Registration paused"
	default:
		response.Operator.Detail = "No IMS registration"
	}
	writeJSON(w, http.StatusOK, response)
}

func (a *App) topologyClients(now time.Time) []TopologyClient {
	selection := a.effectiveSIMAuthSelection(now)
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	result := make([]TopologyClient, 0, len(a.clients))
	for _, client := range a.clients {
		age := now.Sub(client.LastSeen)
		if client.LastSeen.IsZero() || age > topologyRetention(client.Kind) {
			continue
		}
		result = append(result, TopologyClient{
			ID: client.ID, Kind: client.Kind, Name: client.Name,
			AuthAgentID: client.AuthAgentID, HasSIM: client.HasSIM,
			SIMHolder: selection.Mode == "remote" && client.ID == selection.ClientID,
			Online:    age <= clientOnlineWindow, LastSeen: client.LastSeen,
			Cellular: cellularSnapshot(client.Cellular, now, age > clientOnlineWindow),
		})
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Online != result[j].Online {
			return result[i].Online
		}
		if result[i].Kind != result[j].Kind {
			return result[i].Kind < result[j].Kind
		}
		return strings.ToLower(result[i].Name) < strings.ToLower(result[j].Name)
	})
	return result
}

func topologyRetention(kind string) time.Duration {
	if kind == "android" {
		return androidTopologyRetention
	}
	return webTopologyRetention
}

func (a *App) topologySIM(status RuntimeStatus, clients []TopologyClient, now time.Time) TopologySIM {
	selection := a.effectiveSIMAuthSelection(now)
	mode := selection.Mode
	switch mode {
	case "openstick":
		state := "failed"
		detail := "SIM or modem unavailable"
		if status.SIMReady {
			state, detail = "healthy", "SIM accessible over USB"
		}
		return TopologySIM{
			TopologyNode: TopologyNode{State: state, Label: "SIM · 4G modem", Detail: detail, Evidence: "OpenStick ADB readiness", ObservedAt: observedTime(status.UpdatedAt)},
			Mode:         mode, Selected: true, CellularStatus: "not_observed",
		}
	case "remote":
		for _, client := range clients {
			if client.SIMHolder {
				return TopologySIM{
					TopologyNode: TopologyNode{State: "healthy", Label: "SIM · " + client.Name, Detail: "AKA authentication over Tailscale", Evidence: "Active Android agent lease", ObservedAt: observedTime(client.LastSeen)},
					Mode:         mode, HolderClientID: client.ID, AuthAgentID: selection.AgentID, Selected: true,
					CellularStatus: cellularStatus(client.Cellular), Cellular: client.Cellular,
				}
			}
		}
	case "command":
		node := topologyRuntimeNode("SIM · external adapter", status.SIMReady, true, "")
		node.Evidence, node.ObservedAt = "External authentication adapter", observedTime(status.UpdatedAt)
		return TopologySIM{TopologyNode: node, Mode: mode, Selected: true, CellularStatus: "not_observed"}
	default:
		return TopologySIM{TopologyNode: TopologyNode{State: "failed", Label: "No authentication source", Detail: "Connect the modem or enable a phone SIM"}, Mode: "disabled", CellularStatus: "unknown"}
	}
	return TopologySIM{TopologyNode: TopologyNode{State: "failed", Label: "No authentication source", Detail: "Connect the modem or enable a phone SIM"}, Mode: "disabled", CellularStatus: "unknown"}
}

func topologyRuntimeNode(label string, ready, enabled bool, detail string) TopologyNode {
	if !enabled {
		return TopologyNode{State: "idle", Label: label, Detail: "Intentionally disabled"}
	}
	if ready {
		return TopologyNode{State: "healthy", Label: label, Detail: "Operational"}
	}
	return TopologyNode{State: "failed", Label: label, Detail: conciseTopologyDetail(detail)}
}

func conciseTopologyDetail(detail string) string {
	detail = strings.TrimSpace(detail)
	lower := strings.ToLower(detail)
	switch {
	case strings.Contains(lower, "openstick disconnected"), strings.Contains(lower, "device") && strings.Contains(lower, "not found"):
		return "The modem or SIM is unavailable"
	case strings.Contains(lower, "rejected"):
		return "The carrier rejected registration"
	case strings.Contains(lower, "timeout"), strings.Contains(lower, "timed out"):
		return "The check timed out"
	case detail == "":
		return "Unavailable"
	case len([]rune(detail)) > 96:
		return string([]rune(detail)[:93]) + "…"
	default:
		return detail
	}
}

func operatorLabel(realm string) string {
	mcc, mnc := mobileNetworkCodes(realm)
	if mcc != "" && mnc != "" {
		return "Carrier · MCC " + mcc + " / MNC " + mnc
	}
	return "Mobile carrier"
}

func mobileNetworkCodes(realm string) (string, string) {
	match := mobileRealmPattern.FindStringSubmatch(strings.TrimSpace(realm))
	if len(match) == 3 {
		return match[2], match[1]
	}
	return "", ""
}

func observedTime(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	copy := value
	return &copy
}
