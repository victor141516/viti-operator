package app

import (
	"net/http"
	"strings"
)

func (a *App) engineScreen(w http.ResponseWriter, r *http.Request) {
	if !a.engineRequestAuthorized(r) {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var request struct {
		Instance     string `json:"instance"`
		EngineCallID string `json:"call_id"`
	}
	if err := readJSON(r, &request, 4<<10); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	request.Instance = strings.TrimSpace(request.Instance)
	request.EngineCallID = strings.TrimSpace(request.EngineCallID)
	if request.Instance == "" || request.EngineCallID == "" {
		writeError(w, http.StatusBadRequest, "instance and call_id are required")
		return
	}
	call, found := a.store.GetCallByEngineID(request.Instance, request.EngineCallID)
	if !found {
		writeError(w, http.StatusConflict, "incoming call is not available yet")
		return
	}
	action := a.screening.WaitInitial(r.Context(), call.ID)
	writeJSON(w, http.StatusOK, map[string]string{"action": action})
}
