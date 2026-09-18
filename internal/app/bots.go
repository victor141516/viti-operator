package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

const botConfigurationRequestLimit = 64 << 10

type BotConfiguration struct {
	Revision          uint64   `json:"revision"`
	ScreeningPipeline []string `json:"screening_pipeline"`
}

type BotService struct {
	Name            string                     `json:"name"`
	ConnectionID    string                     `json:"connection_id"`
	Capabilities    map[string]json.RawMessage `json:"capabilities"`
	ConnectedAt     time.Time                  `json:"connected_at"`
	AuxiliaryActive bool                       `json:"auxiliary_active"`
}

type BotManagementState struct {
	Configuration BotConfiguration `json:"configuration"`
	Bots          []BotService     `json:"bots"`
}

func (r *integrationRegistry) botSnapshot() []BotService {
	r.mu.Lock()
	defer r.mu.Unlock()
	bots := make([]BotService, 0, len(r.connections))
	for _, connection := range r.connections {
		capabilities := make(map[string]json.RawMessage, len(connection.capabilities))
		for name, options := range connection.capabilities {
			capabilities[name] = append(json.RawMessage(nil), options...)
		}
		bots = append(bots, BotService{
			Name: connection.name, ConnectionID: connection.id, Capabilities: capabilities,
			ConnectedAt: connection.connectedAt, AuxiliaryActive: connection.auxiliaryActive.Load(),
		})
	}
	sort.Slice(bots, func(i, j int) bool { return bots[i].Name < bots[j].Name })
	return bots
}

func (a *App) botManagementState() BotManagementState {
	return BotManagementState{Configuration: a.store.BotConfiguration(), Bots: a.integrations.botSnapshot()}
}

func (a *App) bots(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, a.botManagementState())
}

func validateScreeningPipeline(pipeline []string) error {
	seen := make(map[string]struct{}, len(pipeline))
	for index, name := range pipeline {
		if name != strings.TrimSpace(name) || !integrationNamePattern.MatchString(name) {
			return fmt.Errorf("screening_pipeline[%d] is not a valid service name", index)
		}
		if _, duplicate := seen[name]; duplicate {
			return fmt.Errorf("screening_pipeline contains duplicate service %q", name)
		}
		seen[name] = struct{}{}
	}
	return nil
}

func (a *App) setBotPipeline(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Revision          uint64    `json:"revision"`
		ScreeningPipeline *[]string `json:"screening_pipeline"`
	}
	if err := readJSON(r, &request, botConfigurationRequestLimit); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if request.Revision == 0 || request.ScreeningPipeline == nil {
		writeError(w, http.StatusBadRequest, "revision and screening_pipeline are required")
		return
	}
	if err := validateScreeningPipeline(*request.ScreeningPipeline); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	configuration, matched, changed, err := a.store.UpdateScreeningPipeline(request.Revision, *request.ScreeningPipeline)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not persist the screening pipeline")
		return
	}
	if !matched {
		writeJSON(w, http.StatusConflict, a.botManagementState())
		return
	}
	if changed {
		a.screening.SetPipeline(configuration.ScreeningPipeline)
		a.events.Publish("screening.pipeline.updated", configuration)
	}
	writeJSON(w, http.StatusOK, a.botManagementState())
}

func (a *App) setBotActivation(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PathValue("name"))
	if !integrationNamePattern.MatchString(name) {
		writeError(w, http.StatusBadRequest, "invalid bot name")
		return
	}
	var request struct {
		ConnectionID string `json:"connection_id"`
		Enabled      *bool  `json:"enabled"`
	}
	if err := readJSON(r, &request, botConfigurationRequestLimit); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(request.ConnectionID) == "" || request.Enabled == nil {
		writeError(w, http.StatusBadRequest, "connection_id and enabled are required")
		return
	}
	connection, changed, err := a.integrations.setAuxiliaryActive(name, request.ConnectionID, *request.Enabled)
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if changed && !*request.Enabled && a.serviceMedia != nil {
		if closer, ok := a.serviceMedia.(interface {
			CloseConnectionNonController(connectionID, reason string)
		}); ok {
			closer.CloseConnectionNonController(connection.id, "bot_deactivated")
		}
	}
	if changed {
		a.events.Publish("service.activation.updated", a.integrationServicePayload(connection))
	}
	writeJSON(w, http.StatusOK, a.botManagementState())
}
