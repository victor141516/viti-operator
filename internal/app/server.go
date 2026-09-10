package app

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed web/*
var webAssets embed.FS

var e164Pattern = regexp.MustCompile(`^\+[1-9][0-9]{7,14}$`)

type App struct {
	cfg               Config
	logger            *slog.Logger
	store             *Store
	events            *EventHub
	gateway           TelephonyGateway
	auth              SIMAuthenticator
	adaptiveAuth      *AdaptiveAuthenticator
	broker            *AuthBroker
	diag              *DiagnosticRecorder
	mux               *http.ServeMux
	smsMu             sync.Mutex
	imsMu             sync.Mutex
	simAuthMu         sync.Mutex
	stateMu           sync.RWMutex
	tunnel            engineTunnelState
	clients           map[string]clientPresence
	simAuthAssignment SIMAuthAssignment
}

func New(cfg Config, logger *slog.Logger) (*App, error) {
	if logger == nil {
		logger = slog.Default()
	}
	store, err := OpenStore(cfg.DataFile)
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	if err := store.CloseStaleCalls(time.Now().UTC().Add(-2 * time.Minute)); err != nil {
		return nil, fmt.Errorf("reconcile stale calls: %w", err)
	}
	broker := NewAuthBroker()
	var gateway TelephonyGateway
	switch strings.ToLower(cfg.Telephony.Type) {
	case "", "memory":
		gateway = MemoryGateway{}
	case "command":
		gateway = NewCommandGateway(cfg.Telephony)
	case "asterisk_ami":
		amiGateway, err := NewAMIGateway(cfg.Telephony.AMI)
		if err != nil {
			return nil, err
		}
		gateway = amiGateway
	default:
		return nil, fmt.Errorf("unknown telephony provider %q", cfg.Telephony.Type)
	}
	var fallbackAuthenticator SIMAuthenticator
	fallbackMode := "disabled"
	switch strings.ToLower(cfg.SIMAuth.Type) {
	case "", "disabled":
	case "auto", "openstick":
		if strings.TrimSpace(cfg.SIMAuth.ADB) == "" || strings.TrimSpace(cfg.SIMAuth.ADBSerial) == "" {
			if strings.EqualFold(cfg.SIMAuth.Type, "openstick") {
				return nil, errors.New("OpenStick SIM authentication requires adb and adb_serial")
			}
			break
		}
		fallbackAuthenticator = NewOpenStickAuthenticator(cfg.SIMAuth.ADB, cfg.SIMAuth.ADBSerial)
		fallbackMode = "openstick"
	case "remote":
	case "command":
		if strings.TrimSpace(cfg.SIMAuth.Command.Executable) == "" {
			return nil, errors.New("command SIM authentication requires command.executable")
		}
		fallbackAuthenticator = CommandAuthenticator{Spec: cfg.SIMAuth.Command}
		fallbackMode = "command"
	default:
		return nil, fmt.Errorf("unknown SIM authentication provider %q", cfg.SIMAuth.Type)
	}
	diag, err := NewDiagnosticRecorder(cfg.Diagnostics)
	if err != nil {
		return nil, fmt.Errorf("open diagnostics: %w", err)
	}
	store.SetDiagnostics(diag)
	events := NewEventHub()
	events.SetDiagnostics(diag)
	if instrumented, ok := gateway.(interface{ SetDiagnostics(*DiagnosticRecorder) }); ok {
		instrumented.SetDiagnostics(diag)
	}
	clients := make(map[string]clientPresence)
	for _, client := range store.KnownClients() {
		clients[client.ID] = clientPresence{
			ID: client.ID, Kind: client.Kind, Name: client.Name,
			AuthAgentID: client.AuthAgentID, HasSIM: client.HasSIM, LastSeen: client.LastSeen,
		}
	}
	a := &App{cfg: cfg, logger: logger, store: store, events: events, gateway: gateway, broker: broker, diag: diag, mux: http.NewServeMux(), clients: clients, simAuthAssignment: store.SIMAuthAssignment()}
	if !strings.EqualFold(strings.TrimSpace(cfg.SIMAuth.Type), "disabled") {
		a.adaptiveAuth = &AdaptiveAuthenticator{
			Broker: broker, Fallback: fallbackAuthenticator, FallbackMode: fallbackMode,
			RemoteTimeout: time.Duration(cfg.SIMAuth.RemoteTimeout) * time.Second,
			Select:        a.effectiveSIMAuthSelection,
		}
		a.auth = a.adaptiveAuth
	}
	diag.RecordEvent("manager", "application.started", map[string]any{"telephony_type": cfg.Telephony.Type, "sim_auth_type": cfg.SIMAuth.Type, "voice_enabled": cfg.Voice.Enabled})
	a.routes()
	if source, ok := gateway.(IncomingMessageSource); ok {
		go a.pollIncomingMessages(source)
	}
	return a, nil
}

func (a *App) Handler() http.Handler {
	return securityHeaders(a.traceHTTP(a.mux))
}

func (a *App) routes() {
	a.mux.HandleFunc("GET /api/v1/health", a.health)
	a.mux.Handle("GET /api/v1/status", a.authorized(http.HandlerFunc(a.status)))
	a.mux.Handle("GET /api/v1/topology", a.authorized(http.HandlerFunc(a.topology)))
	a.mux.Handle("POST /api/v1/clients/heartbeat", a.authorized(http.HandlerFunc(a.clientHeartbeat)))
	a.mux.Handle("GET /api/v1/sim-auth/assignment", a.authorized(http.HandlerFunc(a.simAuthAssignmentState)))
	a.mux.Handle("PUT /api/v1/sim-auth/assignment", a.authorized(http.HandlerFunc(a.setSIMAuthAssignment)))
	a.mux.Handle("GET /api/v1/ims-registration", a.authorized(http.HandlerFunc(a.imsRegistration)))
	a.mux.Handle("PUT /api/v1/ims-registration", a.authorized(http.HandlerFunc(a.setIMSRegistration)))
	a.mux.Handle("GET /api/v1/events", a.authorized(http.HandlerFunc(a.waitEvents)))
	a.mux.Handle("GET /api/v1/messages", a.authorized(http.HandlerFunc(a.messages)))
	a.mux.Handle("POST /api/v1/messages", a.authorized(http.HandlerFunc(a.sendMessage)))
	a.mux.Handle("GET /api/v1/calls", a.authorized(http.HandlerFunc(a.calls)))
	a.mux.Handle("GET /api/v1/contacts", a.authorized(http.HandlerFunc(a.contacts)))
	a.mux.Handle("PUT /api/v1/contacts/sources/{client}", a.authorized(http.HandlerFunc(a.replaceContactSource)))
	a.mux.Handle("POST /api/v1/calls", a.authorized(http.HandlerFunc(a.startCall)))
	a.mux.Handle("POST /api/v1/calls/{id}/hangup", a.authorized(http.HandlerFunc(a.hangupCall)))
	a.mux.Handle("GET /api/v1/recording", a.authorized(http.HandlerFunc(a.recordingSettings)))
	a.mux.Handle("PUT /api/v1/recording", a.authorized(http.HandlerFunc(a.setRecordingSettings)))
	a.mux.Handle("POST /api/v1/calls/participants", a.authorized(http.HandlerFunc(a.claimRecordingParticipant)))
	a.mux.Handle("GET /api/v1/calls/{id}/recording", a.authorized(http.HandlerFunc(a.callRecording)))
	a.mux.Handle("GET /api/v1/voice", a.authorized(http.HandlerFunc(a.voiceProvisioning)))
	a.mux.Handle("POST /api/v1/diagnostics/events", a.authorized(http.HandlerFunc(a.diagnosticEvent)))
	a.mux.HandleFunc("GET /sipws", a.voiceWebSocket)
	a.mux.HandleFunc("POST /api/engine/event", a.engineEvent)
	a.mux.HandleFunc("GET /api/engine/recording", a.engineRecordingSettings)
	a.mux.Handle("POST /api/v1/authenticate", a.authorized(http.HandlerFunc(a.authenticate)))
	a.mux.Handle("GET /api/v1/auth/agents/{agent}/challenges", a.authorized(http.HandlerFunc(a.pollAuthChallenge)))
	a.mux.Handle("POST /api/v1/auth/agents/{agent}/challenges/{id}/response", a.authorized(http.HandlerFunc(a.respondAuthChallenge)))
	webRoot, _ := fs.Sub(webAssets, "web")
	a.mux.Handle("/", http.FileServer(http.FS(webRoot)))
}

func (a *App) imsRegistration(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	status, err := a.gateway.Status(ctx)
	state := IMSRegistrationState{
		Enabled:    a.store.IMSRegistrationEnabled(),
		Registered: status.IMSReady,
		Detail:     strings.TrimSpace(status.Detail),
	}
	if err != nil && state.Detail == "" {
		state.Detail = err.Error()
	}
	writeJSON(w, http.StatusOK, state)
}

func (a *App) setIMSRegistration(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Enabled *bool `json:"enabled"`
	}
	if err := readJSON(r, &request, 4<<10); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if request.Enabled == nil {
		writeError(w, http.StatusBadRequest, "enabled is required")
		return
	}
	controller, ok := a.gateway.(IMSRegistrationController)
	if !ok {
		writeError(w, http.StatusNotImplemented, "the telephony provider cannot control IMS registration")
		return
	}
	if !*request.Enabled {
		for _, call := range a.store.Calls() {
			if call.EndedAt == nil {
				writeError(w, http.StatusConflict, "IMS cannot be unregistered during a call")
				return
			}
		}
	}

	a.imsMu.Lock()
	defer a.imsMu.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if err := controller.SetIMSRegistration(ctx, *request.Enabled); err != nil {
		a.diag.RecordEvent("manager.ims", "registration-change.failed", map[string]any{"enabled": *request.Enabled, "error": err.Error()})
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	if err := a.store.SetIMSRegistrationEnabled(*request.Enabled); err != nil {
		// Best effort rollback: a persisted preference and the live registration must not disagree.
		rollbackCtx, rollbackCancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = controller.SetIMSRegistration(rollbackCtx, !*request.Enabled)
		rollbackCancel()
		writeError(w, http.StatusInternalServerError, "could not persist IMS registration preference")
		return
	}
	a.diag.RecordEvent("manager.ims", "registration-change.completed", map[string]any{"enabled": *request.Enabled})
	a.events.Publish("status.updated", map[string]any{"ims_registration_enabled": *request.Enabled})
	detail := "IMS registration stopped"
	if *request.Enabled {
		detail = "IMS registration requested; waiting for carrier confirmation"
	}
	writeJSON(w, http.StatusOK, IMSRegistrationState{Enabled: *request.Enabled, Registered: false, Detail: detail})
}

// engineEvent receives authoritative call lifecycle events from the local
// Asterisk instance. It is deliberately separate from the public client API:
// only the per-installation engine token may write to it.
func (a *App) engineEvent(w http.ResponseWriter, r *http.Request) {
	if !a.engineRequestAuthorized(r) {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var request struct {
		EventID          string   `json:"event_id,omitempty"`
		Instance         string   `json:"instance"`
		Event            string   `json:"event"`
		CallID           string   `json:"call_id,omitempty"`
		TraceID          string   `json:"trace_id,omitempty"`
		Args             []string `json:"args"`
		RecordedAtNS     int64    `json:"recorded_at_ns,omitempty"`
		TunnelGeneration string   `json:"tunnel_generation,omitempty"`
		TunnelSequence   uint64   `json:"tunnel_sequence,omitempty"`
	}
	if err := readJSON(r, &request, 16<<10); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	now := time.Now().UTC()
	request.Instance = strings.TrimSpace(request.Instance)
	request.CallID = strings.TrimSpace(request.CallID)
	request.TraceID = strings.TrimSpace(request.TraceID)
	if a.diag != nil {
		a.diag.RecordTrace("engine", request.Event, request.TraceID, request)
	}
	if request.Event == "recording_track" {
		a.ingestRecordingTrack(w, request.Instance, request.CallID, request.Args, request.RecordedAtNS)
		return
	}
	if request.Event == "sms_protocol" {
		a.ingestSMSProtocolEvent(w, request.Instance, request.Args, request.RecordedAtNS)
		return
	}
	var call Call
	var ok bool
	findCall := func(direction Direction, peer string) (Call, bool) {
		if request.CallID != "" {
			found, exists := a.store.GetCallByEngineID(request.Instance, request.CallID)
			a.diag.RecordTrace("manager.call", "lookup", request.TraceID, map[string]any{"strategy": "engine_call_id", "instance": request.Instance, "engine_call_id": request.CallID, "found": exists, "call": found})
			return found, exists
		}
		found, exists := a.openCall(direction, peer)
		a.diag.RecordTrace("manager.call", "lookup", request.TraceID, map[string]any{"strategy": "legacy_direction_peer", "direction": direction, "peer": peer, "found": exists, "call": found})
		return found, exists
	}
	newCall := func(direction Direction, peer string) (Call, error) {
		id, err := randomID("call")
		if err != nil {
			return Call{}, err
		}
		call := Call{ID: id, TraceID: request.TraceID, EngineCallID: request.CallID, Instance: request.Instance, Direction: direction, Peer: peer, StartedAt: now}
		a.diag.RecordTrace("manager.call", "allocated", request.TraceID, call)
		return call, nil
	}
	switch request.Event {
	case "sms_in":
		a.ingestEngineSMS(w, request.Instance, request.Args, request.RecordedAtNS)
		return
	case "tunnel_up", "tunnel_down":
		generation := strings.TrimSpace(request.TunnelGeneration)
		sequence := request.TunnelSequence
		if generation == "" {
			generation = "legacy"
			if request.RecordedAtNS > 0 {
				sequence = uint64(request.RecordedAtNS)
			} else {
				sequence = uint64(now.UnixNano())
			}
		}
		if !topologyIdentifierPattern.MatchString(generation) || sequence == 0 || request.Instance == "" {
			writeError(w, http.StatusBadRequest, "tunnel events require instance, generation, and sequence")
			return
		}
		if err := a.setEngineTunnelState(request.Instance, generation, sequence, request.Event == "tunnel_up", request.Args, request.RecordedAtNS); err != nil {
			writeError(w, http.StatusInternalServerError, "could not persist tunnel transition")
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	case "call_in", "call_out":
		if len(request.Args) < 1 {
			writeError(w, http.StatusBadRequest, "missing call peer")
			return
		}
		direction := DirectionIncoming
		if request.Event == "call_out" {
			direction = DirectionOutgoing
		}
		peer := strings.TrimSpace(request.Args[0])
		if direction == DirectionIncoming && peer == "" {
			peer = "Private number"
		}
		call, ok = findCall(direction, peer)
		if !ok {
			if peer == "" {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			var err error
			call, err = newCall(direction, peer)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "could not allocate call")
				return
			}
		}
		if call.TraceID == "" {
			call.TraceID = request.TraceID
		}
		if call.EndedAt != nil {
			a.diag.RecordTrace("manager.call", "late-event.ignored", call.TraceID, map[string]any{"event": request.Event, "call": call})
			writeJSON(w, http.StatusOK, call)
			return
		}
		previousState := call.State
		call.State = map[Direction]string{DirectionIncoming: "ringing", DirectionOutgoing: "dialing"}[direction]
		call.UpdatedAt = now
		a.diag.RecordTrace("manager.call", "state.transition", call.TraceID, map[string]any{"event": request.Event, "from": previousState, "to": call.State, "call": call})
	case "call_active":
		if len(request.Args) < 2 {
			writeError(w, http.StatusBadRequest, "invalid active call event")
			return
		}
		direction := normalizeDirection(request.Args[0])
		if direction != DirectionIncoming && direction != DirectionOutgoing {
			writeError(w, http.StatusBadRequest, "invalid call direction")
			return
		}
		peer := strings.TrimSpace(request.Args[1])
		call, ok = findCall(direction, peer)
		if !ok {
			if peer == "" {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			var err error
			call, err = newCall(direction, peer)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "could not allocate call")
				return
			}
		}
		if call.TraceID == "" {
			call.TraceID = request.TraceID
		}
		if call.EndedAt != nil {
			a.diag.RecordTrace("manager.call", "late-event.ignored", call.TraceID, map[string]any{"event": request.Event, "call": call})
			writeJSON(w, http.StatusOK, call)
			return
		}
		previousState := call.State
		call.State = "active"
		if call.AnsweredAt == nil {
			call.AnsweredAt = &now
		}
		call.UpdatedAt = now
		a.diag.RecordTrace("manager.call", "state.transition", call.TraceID, map[string]any{"event": request.Event, "from": previousState, "to": call.State, "call": call})
	case "call_result":
		if len(request.Args) < 4 {
			writeError(w, http.StatusBadRequest, "invalid call result")
			return
		}
		direction := normalizeDirection(request.Args[0])
		if direction != DirectionIncoming && direction != DirectionOutgoing {
			writeError(w, http.StatusBadRequest, "invalid call direction")
			return
		}
		peer := strings.TrimSpace(request.Args[1])
		call, ok = findCall(direction, peer)
		if !ok {
			if peer == "" {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			var err error
			call, err = newCall(direction, peer)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "could not allocate call")
				return
			}
		}
		if call.TraceID == "" {
			call.TraceID = request.TraceID
		}
		if call.EndedAt != nil {
			a.diag.RecordTrace("manager.call", "late-event.ignored", call.TraceID, map[string]any{"event": request.Event, "call": call})
			writeJSON(w, http.StatusOK, call)
			return
		}
		previousState := call.State
		call.State = callResultState(request.Args[2], call.State, request.Args[3])
		call.UpdatedAt = now
		call.EndedAt = &now
		a.diag.RecordTrace("manager.call", "state.transition", call.TraceID, map[string]any{"event": request.Event, "from": previousState, "to": call.State, "dial_status": request.Args[2], "hangup_cause": request.Args[3], "call": call})
	default:
		// Informational engine events do not belong in call or message history.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err := a.store.SaveCall(call); err != nil {
		a.diag.RecordTrace("manager.call", "persist.failed", call.TraceID, map[string]any{"call": call, "error": err.Error()})
		writeError(w, http.StatusInternalServerError, "could not persist call")
		return
	}
	a.events.Publish("call.updated", call)
	a.diag.RecordTrace("manager.call", "event.completed", call.TraceID, map[string]any{"engine_event": request, "call": call})
	writeJSON(w, http.StatusOK, call)
}

func normalizeDirection(value string) Direction {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "in", "incoming":
		return DirectionIncoming
	case "out", "outgoing":
		return DirectionOutgoing
	default:
		return Direction(value)
	}
}

func (a *App) openCall(direction Direction, peer string) (Call, bool) {
	for _, call := range a.store.Calls() {
		if call.Direction == direction && (peer == "" || call.Peer == peer) && call.EndedAt == nil {
			return call, true
		}
	}
	return Call{}, false
}

func callResultState(dialStatus, previousState, hangupCause string) string {
	switch strings.ToUpper(strings.TrimSpace(dialStatus)) {
	case "ANSWER", "ANSWERED":
		return "ended"
	case "BUSY":
		return "busy"
	case "NOANSWER", "CANCEL":
		return "missed"
	default:
		if previousState == "active" && strings.TrimSpace(hangupCause) == "16" {
			return "ended"
		}
		return "failed"
	}
}

func (a *App) voiceProvisioning(w http.ResponseWriter, r *http.Request) {
	if !a.cfg.Voice.Enabled {
		writeError(w, http.StatusServiceUnavailable, "voice is disabled")
		return
	}
	webSocketURL := strings.TrimSpace(a.cfg.Voice.WebSocketURL)
	domain := strings.TrimSpace(a.cfg.Voice.Domain)
	if webSocketURL == "" {
		scheme := "ws"
		if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
			scheme = "wss"
		}
		webSocketURL = scheme + "://" + r.Host + "/sipws"
	}
	if domain == "" {
		domain = r.Host
		if host, _, err := net.SplitHostPort(r.Host); err == nil {
			domain = host
		}
	}
	writeJSON(w, http.StatusOK, struct {
		Enabled            bool              `json:"enabled"`
		WebSocketURL       string            `json:"websocket_url"`
		Domain             string            `json:"domain"`
		Username           string            `json:"username"`
		Password           string            `json:"password"`
		ICEServers         []ICEServerConfig `json:"ice_servers,omitempty"`
		DiagnosticsEnabled bool              `json:"diagnostics_enabled"`
	}{true, webSocketURL, domain, a.cfg.Voice.Username, a.cfg.Voice.Password, a.cfg.Voice.ICEServers, a.cfg.Diagnostics.Enabled})
}

func (a *App) voiceWebSocket(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	connectionID, err := randomID("sipws")
	if err != nil {
		connectionID = fmt.Sprintf("sipws-%d", time.Now().UnixNano())
	}
	a.diag.RecordEvent("manager.websocket", "upgrade.requested", map[string]any{"connection_id": connectionID, "remote_address": r.RemoteAddr, "headers": r.Header, "url": r.URL.String()})
	if !a.cfg.Voice.Enabled || strings.TrimSpace(a.cfg.Voice.BackendWebSocketURL) == "" {
		a.diag.RecordEvent("manager.websocket", "upgrade.rejected", map[string]any{"connection_id": connectionID, "reason": "disabled"})
		writeError(w, http.StatusServiceUnavailable, "voice websocket is disabled")
		return
	}
	target, err := url.Parse(a.cfg.Voice.BackendWebSocketURL)
	if err != nil {
		a.diag.RecordEvent("manager.websocket", "upgrade.rejected", map[string]any{"connection_id": connectionID, "reason": "invalid-backend", "error": err.Error()})
		writeError(w, http.StatusInternalServerError, "invalid voice websocket backend")
		return
	}
	// net/http's reverse proxy performs WebSocket upgrades over an HTTP(S)
	// transport. Accept the natural ws:// configuration form, but translate it
	// for the upstream round trip while preserving the Upgrade headers.
	switch target.Scheme {
	case "ws":
		target.Scheme = "http"
	case "wss":
		target.Scheme = "https"
	case "http", "https":
	default:
		writeError(w, http.StatusInternalServerError, "invalid voice websocket backend scheme")
		return
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	originalDirector := proxy.Director
	proxy.Director = func(request *http.Request) {
		originalDirector(request)
		request.URL.Path = target.Path
		request.Host = target.Host
		a.diag.RecordEvent("manager.websocket", "upstream.requested", map[string]any{"connection_id": connectionID, "target": target.String(), "headers": request.Header})
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		a.logger.Warn("voice websocket proxy failed", "error", err)
		a.diag.RecordEvent("manager.websocket", "upstream.failed", map[string]any{"connection_id": connectionID, "error": err.Error()})
		writeError(w, http.StatusBadGateway, "voice websocket unavailable")
	}
	proxy.ServeHTTP(w, r)
	a.diag.RecordEvent("manager.websocket", "connection.closed", map[string]any{"connection_id": connectionID, "duration_ns": time.Since(started).Nanoseconds()})
}

func (a *App) authorized(next http.Handler) http.Handler {
	if a.cfg.APIKey == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		provided := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if provided == "" {
			provided = r.Header.Get("X-API-Key")
		}
		if subtle.ConstantTimeCompare([]byte(provided), []byte(a.cfg.APIKey)) != 1 {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (a *App) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) status(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	status, _ := a.runtimeStatus(ctx)
	// The manager itself is healthy even when the removable modem is absent.
	// Availability is represented in the body so web and Android clients can
	// render the disconnected state instead of treating it as a server outage.
	writeJSON(w, http.StatusOK, status)
}

func (a *App) messages(w http.ResponseWriter, _ *http.Request) {
	items := a.store.Messages()
	contacts := a.store.Contacts("")
	for index := range items {
		peer := items[index].From
		if items[index].Direction == DirectionOutgoing {
			peer = items[index].To
		}
		if contact, found := resolveContact(contacts, peer); found {
			items[index].ContactID = contact.ID
			items[index].ContactName = contact.Name
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"messages": items})
}

func (a *App) sendMessage(w http.ResponseWriter, r *http.Request) {
	var request struct {
		To   string `json:"to"`
		Text string `json:"text"`
	}
	if err := readJSON(r, &request, 32<<10); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	request.To = strings.TrimSpace(request.To)
	request.Text = strings.TrimSpace(request.Text)
	if !e164Pattern.MatchString(request.To) || request.Text == "" || len([]rune(request.Text)) > 1000 {
		writeError(w, http.StatusBadRequest, "use an E.164 destination and 1–1000 characters")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
	defer cancel()
	if status, err := a.runtimeStatus(ctx); err != nil {
		a.diag.RecordEvent("manager.sms", "send.blocked-unavailable", status)
		writeError(w, http.StatusServiceUnavailable, runtimeUnavailableMessage(status))
		return
	}
	id, err := randomID("sms")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not allocate message")
		return
	}
	now := time.Now().UTC()
	deadline := now.Add(smsDeliveryReportWindow)
	message := Message{
		ID: id, Direction: DirectionOutgoing, To: request.To, Text: request.Text,
		State: "sending", DeliveryState: "pending", StatusDetail: "Queued for local submission.",
		DeliveryReportDeadline: &deadline, CreatedAt: now, UpdatedAt: now,
	}
	if err := a.store.SaveMessage(message); err != nil {
		writeError(w, http.StatusInternalServerError, "could not persist message")
		return
	}
	a.events.Publish("message.updated", message)
	go a.deliverMessage(message)
	writeJSON(w, http.StatusAccepted, message)
}

func (a *App) deliverMessage(message Message) {
	// Both clients may submit concurrently, but the single IMS MESSAGE channel
	// is serialized here. Requests are accepted immediately and remain visible
	// as "sending" while queued.
	a.smsMu.Lock()
	defer a.smsMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	result, err := a.gateway.SendSMS(ctx, message.ID, message.To, message.Text)
	updated, changed, persistErr := a.store.UpdateMessage(message.ID, func(current *Message) bool {
		return applySMSSubmitResult(current, result, err, time.Now().UTC())
	})
	if persistErr != nil {
		a.logger.Warn("persist SMS submission result", "message_id", message.ID, "error", persistErr)
	} else if changed {
		a.events.Publish("message.updated", updated)
	}
	if err != nil && !errors.Is(err, ErrDeliveryUnconfirmed) {
		a.logger.Warn("SMS send failed", "message_id", message.ID, "error", err)
	}
}

func (a *App) ingestSMSProtocolEvent(w http.ResponseWriter, instance string, args []string, recordedAtNS int64) {
	if len(args) < 9 {
		writeError(w, http.StatusBadRequest, "invalid SMS protocol event")
		return
	}
	parseOptional := func(name, raw string) (*int, error) {
		raw = strings.TrimSpace(raw)
		if raw == "" || raw == "-1" {
			return nil, nil
		}
		value, err := strconv.ParseInt(raw, 0, 16)
		if err != nil || value < 0 || value > 255 {
			return nil, fmt.Errorf("invalid %s", name)
		}
		parsed := int(value)
		return &parsed, nil
	}
	part, err := strconv.Atoi(strings.TrimSpace(args[3]))
	if err != nil || part <= 0 {
		writeError(w, http.StatusBadRequest, "invalid SMS part")
		return
	}
	parts, err := strconv.Atoi(strings.TrimSpace(args[4]))
	if err != nil || parts < part {
		writeError(w, http.StatusBadRequest, "invalid SMS part count")
		return
	}
	rpReference, err := parseOptional("RP reference", args[5])
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	tpReference, err := parseOptional("TP reference", args[6])
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	status, err := parseOptional("TP status", args[7])
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	cause, err := parseOptional("RP cause", args[8])
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var serviceCentreTime *time.Time
	if len(args) > 9 {
		raw := strings.TrimSpace(args[9])
		if raw != "" && raw != "-1" {
			seconds, parseErr := strconv.ParseInt(raw, 10, 64)
			if parseErr != nil || seconds <= 0 {
				writeError(w, http.StatusBadRequest, "invalid service-centre timestamp")
				return
			}
			parsed := time.Unix(seconds, 0).UTC()
			serviceCentreTime = &parsed
		}
	}
	event := SMSProtocolEvent{
		Instance: strings.TrimSpace(instance), Phase: strings.TrimSpace(args[0]),
		MessageID: strings.TrimSpace(args[1]), Destination: strings.TrimSpace(args[2]),
		Part: part, Parts: parts, RPReference: rpReference, TPReference: tpReference,
		Status: status, Cause: cause, ServiceCentreTime: serviceCentreTime,
	}
	eventTime := time.Now().UTC()
	if recordedAtNS > 0 {
		eventTime = time.Unix(0, recordedAtNS).UTC()
	}
	message, changed, err := a.store.ApplySMSProtocolEvent(event, eventTime)
	if errors.Is(err, ErrSMSReportNotCorrelated) {
		// The durable engine outbox retains an early response until its later
		// submitted event establishes the persistent reference mapping.
		a.logger.Warn("SMS protocol event is waiting for correlation", "event", event, "error", err)
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if errors.Is(err, ErrSMSReportAmbiguous) {
		a.logger.Warn("SMS protocol event was not applied", "event", event, "error", err)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if changed {
		a.events.Publish("message.updated", message)
	}
	writeJSON(w, http.StatusOK, message)
}

func (a *App) calls(w http.ResponseWriter, _ *http.Request) {
	items := a.callsWithRecordingTiming()
	contacts := a.store.Contacts("")
	for index := range items {
		if contact, found := resolveContact(contacts, items[index].Peer); found {
			items[index].ContactID = contact.ID
			items[index].ContactName = contact.Name
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"calls": items})
}

func (a *App) startCall(w http.ResponseWriter, r *http.Request) {
	traceID := strings.TrimSpace(r.Header.Get("X-Viti-Trace-ID"))
	var request struct {
		To string `json:"to"`
	}
	if err := readJSON(r, &request, 4<<10); err != nil || !e164Pattern.MatchString(strings.TrimSpace(request.To)) {
		writeError(w, http.StatusBadRequest, "use an E.164 destination")
		return
	}
	statusCtx, statusCancel := context.WithTimeout(r.Context(), 4*time.Second)
	defer statusCancel()
	if status, err := a.runtimeStatus(statusCtx); err != nil {
		a.diag.RecordTrace("manager.call", "api.start.blocked-unavailable", traceID, status)
		writeError(w, http.StatusServiceUnavailable, runtimeUnavailableMessage(status))
		return
	}
	for _, existing := range a.store.Calls() {
		if existing.EndedAt == nil && time.Since(existing.UpdatedAt) < 2*time.Minute {
			writeError(w, http.StatusConflict, "the line is already in use")
			return
		}
	}
	id, err := randomID("call")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not allocate call")
		return
	}
	now := time.Now().UTC()
	if traceID == "" {
		traceID = id
	}
	call := Call{ID: id, TraceID: traceID, Direction: DirectionOutgoing, Peer: strings.TrimSpace(request.To), State: "dialing", StartedAt: now, UpdatedAt: now}
	a.diag.RecordTrace("manager.call", "api.start.accepted", traceID, call)
	if err := a.store.SaveCall(call); err != nil {
		writeError(w, http.StatusInternalServerError, "could not persist call")
		return
	}
	a.events.Publish("call.updated", call)
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	if err := a.gateway.StartCall(ctx, call.ID, call.Peer); err != nil {
		a.diag.RecordTrace("manager.call", "gateway.start.failed", traceID, map[string]any{"call": call, "error": err.Error()})
		call.State = "failed"
		call.UpdatedAt = time.Now().UTC()
		_ = a.store.SaveCall(call)
		a.events.Publish("call.updated", call)
		writeJSON(w, http.StatusBadGateway, call)
		return
	}
	a.diag.RecordTrace("manager.call", "gateway.start.completed", traceID, call)
	writeJSON(w, http.StatusCreated, call)
}

func (a *App) hangupCall(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	call, ok := a.store.GetCall(id)
	if !ok {
		writeError(w, http.StatusNotFound, "call not found")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	a.diag.RecordTrace("manager.call", "api.hangup.requested", call.TraceID, call)
	if err := a.gateway.HangupCall(ctx, id); err != nil {
		a.diag.RecordTrace("manager.call", "gateway.hangup.failed", call.TraceID, map[string]any{"call": call, "error": err.Error()})
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	now := time.Now().UTC()
	call.State = "ended"
	call.UpdatedAt = now
	call.EndedAt = &now
	_ = a.store.SaveCall(call)
	a.events.Publish("call.updated", call)
	a.diag.RecordTrace("manager.call", "api.hangup.completed", call.TraceID, call)
	writeJSON(w, http.StatusOK, call)
}

func (a *App) waitEvents(w http.ResponseWriter, r *http.Request) {
	after, _ := strconv.ParseUint(r.URL.Query().Get("after"), 10, 64)
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	writeJSON(w, http.StatusOK, map[string]any{"events": a.events.Wait(ctx, after)})
}

func (a *App) authenticate(w http.ResponseWriter, r *http.Request) {
	if a.auth == nil {
		writeError(w, http.StatusServiceUnavailable, "SIM authentication is disabled")
		return
	}
	var request struct {
		RAND string `json:"rand"`
		AUTN string `json:"autn"`
	}
	if err := readJSON(r, &request, 4<<10); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	rand16, err := base64.RawStdEncoding.DecodeString(request.RAND)
	if err != nil || len(rand16) != 16 {
		writeError(w, http.StatusBadRequest, "RAND must be 16-byte unpadded Base64")
		return
	}
	autn16, err := base64.RawStdEncoding.DecodeString(request.AUTN)
	if err != nil || len(autn16) != 16 {
		writeError(w, http.StatusBadRequest, "AUTN must be 16-byte unpadded Base64")
		return
	}
	result, err := a.auth.AuthenticateAKA(r.Context(), rand16, autn16)
	if err != nil {
		a.logger.Warn("SIM authentication failed", "error", err)
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	response := AuthResponse{
		RES: base64.RawStdEncoding.EncodeToString(result.RES),
		CK:  base64.RawStdEncoding.EncodeToString(result.CK),
		IK:  base64.RawStdEncoding.EncodeToString(result.IK),
	}
	if len(result.AUTS) > 0 {
		response = AuthResponse{AUTS: base64.RawStdEncoding.EncodeToString(result.AUTS)}
	}
	writeJSON(w, http.StatusOK, response)
}

func (a *App) pollAuthChallenge(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 7*time.Second)
	defer cancel()
	challenge := a.broker.Poll(ctx, r.PathValue("agent"))
	if challenge == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	a.logger.Info("SIM authentication challenge delivered", "agent", r.PathValue("agent"))
	writeJSON(w, http.StatusOK, challenge)
}

func (a *App) respondAuthChallenge(w http.ResponseWriter, r *http.Request) {
	var response AuthResponse
	if err := readJSON(r, &response, 8<<10); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := a.broker.Respond(r.PathValue("agent"), r.PathValue("id"), response); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	a.logger.Info("SIM authentication response received", "agent", r.PathValue("agent"))
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) pollIncomingMessages(source IncomingMessageSource) {
	ticker := time.NewTicker(time.Duration(a.cfg.Telephony.PollSeconds) * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		messages, err := source.IncomingMessages(ctx)
		cancel()
		if err != nil {
			a.logger.Debug("incoming message poll failed", "error", err)
			continue
		}
		for _, message := range messages {
			if err := a.store.SaveMessage(message); err != nil {
				a.logger.Warn("persist incoming message", "error", err)
				continue
			}
			a.events.Publish("message.received", message)
		}
	}
}

func readJSON(r *http.Request, target any, limit int64) error {
	decoder := json.NewDecoder(io.LimitReader(r.Body, limit))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Permissions-Policy", "camera=(), geolocation=(), payment=()")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; connect-src 'self' ws: wss:; media-src 'self' blob:; img-src 'self' data:; style-src 'self' 'unsafe-inline'; script-src 'self'; base-uri 'none'; frame-ancestors 'self'")
		next.ServeHTTP(w, r)
	})
}
