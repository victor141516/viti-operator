package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

const (
	defaultScreeningDecisionTimeout = 2 * time.Second
	defaultScreeningMediaTimeout    = 2 * time.Second
	screeningHumanRingTimeout       = 60 * time.Second
	screeningOutputLimit            = 64 << 10
)

type screeningCapabilityOptions struct {
	DecisionTimeoutMS   int64 `json:"decision_timeout_ms,omitempty"`
	ProcessingTimeoutMS int64 `json:"processing_timeout_ms,omitempty"`
	MediaTimeoutMS      int64 `json:"media_timeout_ms,omitempty"`
}

func (options screeningCapabilityOptions) normalized() screeningCapabilityOptions {
	if options.DecisionTimeoutMS == 0 {
		options.DecisionTimeoutMS = defaultScreeningDecisionTimeout.Milliseconds()
	}
	if options.ProcessingTimeoutMS == 0 {
		options.ProcessingTimeoutMS = -1
	}
	if options.MediaTimeoutMS == 0 {
		options.MediaTimeoutMS = defaultScreeningMediaTimeout.Milliseconds()
	}
	return options
}

func (options screeningCapabilityOptions) validate() error {
	options = options.normalized()
	if options.DecisionTimeoutMS <= 0 {
		return errors.New("decision_timeout_ms must be positive")
	}
	if options.ProcessingTimeoutMS != -1 && options.ProcessingTimeoutMS <= 0 {
		return errors.New("processing_timeout_ms must be positive or -1")
	}
	if options.MediaTimeoutMS <= 0 {
		return errors.New("media_timeout_ms must be positive")
	}
	return nil
}

type screeningDecisionPayload struct {
	Revision uint64          `json:"revision"`
	Action   string          `json:"action"`
	Output   json.RawMessage `json:"output,omitempty"`
}

type screeningDecisionReceipt struct {
	CallID        string `json:"call_id"`
	Revision      uint64 `json:"revision"`
	StageIndex    int    `json:"stage_index"`
	State         string `json:"state"`
	Cached        bool   `json:"cached,omitempty"`
	Authoritative bool   `json:"authoritative,omitempty"`
}

type screeningCommandError struct {
	code    string
	message string
}

func (err *screeningCommandError) Error() string { return err.message }

func screeningError(code, format string, args ...any) error {
	return &screeningCommandError{code: code, message: fmt.Sprintf(format, args...)}
}

func screeningErrorCode(err error) string {
	var commandError *screeningCommandError
	if errors.As(err, &commandError) {
		return commandError.code
	}
	return "screening_failed"
}

type screeningCallbacks struct {
	closeController func(serviceName, callID, reason string)
	ringClients     func(context.Context, Call) error
	hangup          func(context.Context, Call) error
}

type screeningManager struct {
	mu          sync.Mutex
	pipeline    []string
	store       *Store
	events      *EventHub
	services    *integrationRegistry
	callbacks   screeningCallbacks
	sessions    map[string]*screeningSession
	recovered   map[string]*screeningRecovery
	ringTimeout time.Duration
}

type screeningRecovery struct {
	timer *time.Timer
}

type screeningSession struct {
	call                   Call
	record                 *CallScreeningRecord
	current                int
	phase                  string
	targetConnectionID     string
	controllerSessionID    string
	carrierAnswerRequested bool
	initial                chan string
	initialOnce            sync.Once
	decisionTimer          *time.Timer
	mediaTimer             *time.Timer
	processingTimer        *time.Timer
	processingDeadline     time.Time
	ringTimer              *time.Timer
	cached                 map[string]cachedScreeningDecision
	decisionsClosed        bool
	terminal               bool
	initialAction          string
}

type cachedScreeningDecision struct {
	serviceName  string
	connectionID string
	commandID    string
	payload      screeningDecisionPayload
	at           time.Time
}

func newScreeningManager(pipeline []string, store *Store, events *EventHub, services *integrationRegistry, callbacks screeningCallbacks) *screeningManager {
	return &screeningManager{
		pipeline: append([]string(nil), pipeline...), store: store, events: events, services: services,
		callbacks: callbacks, sessions: make(map[string]*screeningSession), recovered: make(map[string]*screeningRecovery),
		ringTimeout: screeningHumanRingTimeout,
	}
}

func (m *screeningManager) StartIncoming(call Call) {
	if call.Direction != DirectionIncoming || call.EndedAt != nil {
		return
	}
	if len(m.pipeline) == 0 {
		m.events.Publish("call.incoming", screeningCallPayload(call))
		return
	}
	m.mu.Lock()
	if _, exists := m.sessions[call.ID]; exists {
		m.mu.Unlock()
		return
	}
	now := time.Now().UTC()
	record := &CallScreeningRecord{
		Pipeline: append([]string(nil), m.pipeline...), StartedAt: now, ContextRevision: 1,
		CurrentStage: 0, State: "screening", Answered: call.AnsweredAt != nil,
	}
	session := &screeningSession{
		call: call, record: record, current: 0, phase: "offering", initial: make(chan string, 1),
		cached: make(map[string]cachedScreeningDecision),
	}
	m.sessions[call.ID] = session
	m.persistLocked(session)
	m.events.Publish("call.incoming", screeningCallPayload(call))
	m.advanceLocked(session)
	m.mu.Unlock()
}

func screeningCallPayload(call Call) map[string]any {
	return map[string]any{
		"call_id": call.ID, "trace_id": call.TraceID, "engine_call_id": call.EngineCallID,
		"instance": call.Instance, "direction": call.Direction, "peer": call.Peer,
		"state": call.State, "started_at": call.StartedAt,
	}
}

func (m *screeningManager) WaitInitial(ctx context.Context, callID string) string {
	action, ready := m.pollInitial(ctx, callID)
	if ready {
		return action
	}
	m.failOpen(callID, "engine_wait_lost")
	return "ring_clients"
}

func (m *screeningManager) pollInitial(ctx context.Context, callID string) (string, bool) {
	m.mu.Lock()
	session := m.sessions[callID]
	if session == nil {
		m.mu.Unlock()
		if call, found := m.store.GetCall(callID); found && call.EndedAt != nil {
			return "hangup", true
		}
		return "ring_clients", true
	}
	initial := session.initial
	if session.initialAction != "" {
		action := session.initialAction
		m.mu.Unlock()
		return action, true
	}
	m.mu.Unlock()
	select {
	case <-initial:
		m.mu.Lock()
		action := session.initialAction
		m.mu.Unlock()
		return action, action != ""
	case <-ctx.Done():
		return "", false
	}
}

func (m *screeningManager) failOpen(callID, reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	session := m.sessions[callID]
	if session == nil || session.terminal || session.decisionsClosed {
		return
	}
	if session.current < len(session.record.Pipeline) {
		now := time.Now().UTC()
		m.completeCurrentStageLocked(session, reason, now)
	}
	m.finishRingLocked(session, "viti", reason)
}

func (m *screeningManager) Decide(serviceName, connectionID, commandID, callID string, payload screeningDecisionPayload, canHangup bool) (screeningDecisionReceipt, error) {
	payload.Action = strings.ToLower(strings.TrimSpace(payload.Action))
	if !screeningDecisionAction(payload.Action) {
		return screeningDecisionReceipt{}, screeningError("invalid_payload", "action must be take_control, next, ring_clients, or hangup")
	}
	if len(payload.Output) > screeningOutputLimit {
		return screeningDecisionReceipt{}, screeningError("output_too_large", "output exceeds 64 KiB")
	}
	if len(bytes.TrimSpace(payload.Output)) != 0 && !json.Valid(payload.Output) {
		return screeningDecisionReceipt{}, screeningError("invalid_payload", "output must be valid JSON")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	session := m.sessions[callID]
	if session == nil {
		if call, found := m.store.GetCall(callID); found && call.EndedAt != nil {
			return screeningDecisionReceipt{}, screeningError("already_ended", "call has already ended")
		}
		return screeningDecisionReceipt{}, screeningError("screening_not_found", "call has no active screening pipeline")
	}
	if session.terminal {
		return screeningDecisionReceipt{}, screeningError("already_ended", "call has already ended")
	}
	if payload.Action == "hangup" && canHangup {
		if payload.Revision != session.record.ContextRevision {
			return screeningDecisionReceipt{}, screeningError("stale_revision", "decision revision is stale")
		}
		m.appendUpdateLocked(session, session.current, serviceName, commandID, payload, time.Now().UTC())
		m.finishHangupLocked(session, serviceName, "service_requested")
		return m.receiptLocked(session, session.current, false, true), nil
	}
	stageIndex := indexOfScreeningService(session.record.Pipeline, serviceName)
	if stageIndex < 0 {
		return screeningDecisionReceipt{}, screeningError("wrong_service", "service is not part of this call's pipeline snapshot")
	}
	if payload.Revision != session.record.ContextRevision {
		return screeningDecisionReceipt{}, screeningError("stale_revision", "decision revision is stale")
	}
	if session.decisionsClosed {
		return screeningDecisionReceipt{}, screeningError("pipeline_complete", "screening decisions are already closed")
	}
	if stageIndex < session.current {
		return screeningDecisionReceipt{}, screeningError("stage_complete", "screening stage has already completed")
	}
	if stageIndex > session.current {
		key := screeningCacheKey(stageIndex, payload.Revision)
		if _, exists := session.cached[key]; exists {
			return screeningDecisionReceipt{}, screeningError("decision_conflict", "this stage already cached a decision for the revision")
		}
		session.cached[key] = cachedScreeningDecision{
			serviceName: serviceName, connectionID: connectionID, commandID: commandID,
			payload: payload, at: time.Now().UTC(),
		}
		return m.receiptLocked(session, stageIndex, true, false), nil
	}
	if connectionID != session.targetConnectionID {
		return screeningDecisionReceipt{}, screeningError("stale_connection_generation", "decision belongs to a stale service connection")
	}
	if session.phase == "waiting_media" || session.phase == "waiting_active" {
		return screeningDecisionReceipt{}, screeningError("controller_not_ready", "the controlling media session is not active yet")
	}
	if session.phase == "controlling" && payload.Action == "take_control" {
		return screeningDecisionReceipt{}, screeningError("invalid_action", "the stage already controls the call")
	}
	if session.phase != "offering" && session.phase != "controlling" {
		return screeningDecisionReceipt{}, screeningError("invalid_state", "stage cannot accept a decision in its current state")
	}
	m.applyCurrentDecisionLocked(session, cachedScreeningDecision{
		serviceName: serviceName, connectionID: connectionID, commandID: commandID,
		payload: payload, at: time.Now().UTC(),
	})
	return m.receiptLocked(session, stageIndex, false, true), nil
}

func screeningDecisionAction(action string) bool {
	switch action {
	case "take_control", "next", "ring_clients", "hangup":
		return true
	default:
		return false
	}
}

func indexOfScreeningService(pipeline []string, service string) int {
	for index, candidate := range pipeline {
		if candidate == service {
			return index
		}
	}
	return -1
}

func screeningCacheKey(stage int, revision uint64) string {
	return fmt.Sprintf("%d:%d", stage, revision)
}

func (m *screeningManager) receiptLocked(session *screeningSession, stage int, cached, authoritative bool) screeningDecisionReceipt {
	return screeningDecisionReceipt{
		CallID: session.call.ID, Revision: session.record.ContextRevision, StageIndex: stage,
		State: session.record.State, Cached: cached, Authoritative: authoritative,
	}
}

func (m *screeningManager) applyCurrentDecisionLocked(session *screeningSession, decision cachedScreeningDecision) {
	m.stopTimer(&session.decisionTimer)
	m.stopTimer(&session.processingTimer)
	now := decision.at
	m.appendUpdateLocked(session, session.current, decision.serviceName, decision.commandID, decision.payload, now)
	switch decision.payload.Action {
	case "take_control":
		session.phase = "waiting_media"
		session.record.State = "waiting_media"
		stage := m.currentStageLocked(session)
		stage.Outcome = "waiting_media"
		session.processingDeadline = time.Time{}
		duration := screeningTimeout(stage.MediaTimeoutMS)
		callID, stageIndex, revision := session.call.ID, session.current, session.record.ContextRevision
		session.mediaTimer = time.AfterFunc(duration, func() { m.mediaTimeout(callID, stageIndex, revision) })
		m.persistLocked(session)
	case "next":
		m.completeCurrentStageLocked(session, "next", now)
		m.advanceLocked(session)
	case "ring_clients":
		m.completeCurrentStageLocked(session, "ring_clients", now)
		m.finishRingLocked(session, decision.serviceName, "service_requested")
	case "hangup":
		m.completeCurrentStageLocked(session, "hangup", now)
		m.finishHangupLocked(session, decision.serviceName, "service_requested")
	}
}

func (m *screeningManager) appendUpdateLocked(session *screeningSession, stageIndex int, serviceName, commandID string, payload screeningDecisionPayload, at time.Time) {
	if stageIndex < 0 || stageIndex >= len(session.record.Pipeline) {
		return
	}
	stage := m.stageRecordLocked(session, stageIndex)
	stage.Updates = append(stage.Updates, ScreeningUpdate{
		ServiceName: serviceName, CommandID: commandID, Action: payload.Action,
		Output: append(json.RawMessage(nil), payload.Output...), At: at,
	})
	if screeningOutputPresent(payload.Output) {
		session.record.ContextRevision++
	}
}

func screeningOutputPresent(output json.RawMessage) bool {
	trimmed := bytes.TrimSpace(output)
	return len(trimmed) != 0 && !bytes.Equal(trimmed, []byte("null"))
}

func (m *screeningManager) advanceLocked(session *screeningSession) {
	for session.current < len(session.record.Pipeline) {
		serviceName := session.record.Pipeline[session.current]
		connection, options, available := m.services.screeningService(serviceName)
		if !available {
			now := time.Now().UTC()
			stage := m.stageRecordWithOptionsLocked(session, session.current, serviceName, options)
			stage.Outcome, stage.CompletedAt = "skipped_unavailable", &now
			session.current++
			session.record.CurrentStage = session.current
			m.persistLocked(session)
			continue
		}
		session.phase = "offering"
		session.targetConnectionID = connection.id
		session.controllerSessionID = ""
		session.record.CurrentStage = session.current
		session.record.State = "screening"
		now := time.Now().UTC()
		stage := m.stageRecordWithOptionsLocked(session, session.current, serviceName, options)
		stage.Context = screeningContext(session.record.Answered)
		stage.ContextRevision = session.record.ContextRevision
		stage.OfferedAt = &now
		deadline := now.Add(screeningTimeout(stage.DecisionTimeoutMS))
		m.persistLocked(session)
		m.events.Publish("screening.offer", map[string]any{
			"call_id": session.call.ID, "trace_id": session.call.TraceID, "target_service": serviceName,
			"stage_index": session.current, "revision": session.record.ContextRevision,
			"context": stage.Context, "deadline": deadline, "call": screeningCallPayload(session.call),
			"prior_results": cloneCallScreening(session.record).Stages[:len(session.record.Stages)-1],
		})
		if cached, exists := session.cached[screeningCacheKey(session.current, session.record.ContextRevision)]; exists {
			delete(session.cached, screeningCacheKey(session.current, session.record.ContextRevision))
			if cached.connectionID == connection.id {
				m.applyCurrentDecisionLocked(session, cached)
				return
			}
		}
		callID, stageIndex := session.call.ID, session.current
		revision, connectionID := session.record.ContextRevision, connection.id
		session.decisionTimer = time.AfterFunc(time.Until(deadline), func() { m.decisionTimeout(callID, stageIndex, revision, connectionID) })
		return
	}
	m.finishRingLocked(session, "viti", "pipeline_exhausted")
}

func screeningContext(answered bool) string {
	if answered {
		return "active"
	}
	return "incoming"
}

func (m *screeningManager) stageRecordWithOptionsLocked(session *screeningSession, index int, serviceName string, options screeningCapabilityOptions) *ScreeningStageRecord {
	for len(session.record.Stages) <= index {
		nextIndex := len(session.record.Stages)
		nextName := session.record.Pipeline[nextIndex]
		session.record.Stages = append(session.record.Stages, ScreeningStageRecord{Index: nextIndex, ServiceName: nextName})
	}
	stage := &session.record.Stages[index]
	if stage.ServiceName != "" && (stage.OfferedAt != nil || stage.CompletedAt != nil) {
		return stage
	}
	options = options.normalized()
	stage.ServiceName = serviceName
	stage.DecisionTimeoutMS = options.DecisionTimeoutMS
	stage.ProcessingTimeoutMS = options.ProcessingTimeoutMS
	stage.MediaTimeoutMS = options.MediaTimeoutMS
	stage.Context = screeningContext(session.record.Answered)
	stage.ContextRevision = session.record.ContextRevision
	return stage
}

func (m *screeningManager) stageRecordLocked(session *screeningSession, index int) *ScreeningStageRecord {
	if index >= 0 && index < len(session.record.Stages) {
		return &session.record.Stages[index]
	}
	options := screeningCapabilityOptions{}.normalized()
	if index >= 0 && index < len(session.record.Pipeline) {
		if _, currentOptions, ok := m.services.screeningService(session.record.Pipeline[index]); ok {
			options = currentOptions
		}
		return m.stageRecordWithOptionsLocked(session, index, session.record.Pipeline[index], options)
	}
	return nil
}

func (m *screeningManager) currentStageLocked(session *screeningSession) *ScreeningStageRecord {
	return m.stageRecordLocked(session, session.current)
}

func (m *screeningManager) completeCurrentStageLocked(session *screeningSession, outcome string, at time.Time) {
	stage := m.currentStageLocked(session)
	stage.Outcome, stage.CompletedAt = outcome, &at
	if session.phase == "controlling" || session.phase == "waiting_active" || session.phase == "waiting_media" {
		serviceName, callID := stage.ServiceName, session.call.ID
		if m.callbacks.closeController != nil {
			go m.callbacks.closeController(serviceName, callID, "screening_"+outcome)
		}
	}
	m.stopTimer(&session.mediaTimer)
	m.stopTimer(&session.processingTimer)
	session.processingDeadline = time.Time{}
	session.current++
	session.record.CurrentStage = session.current
	session.targetConnectionID = ""
	session.controllerSessionID = ""
	m.persistLocked(session)
}

func (m *screeningManager) decisionTimeout(callID string, stageIndex int, revision uint64, connectionID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	session := m.sessions[callID]
	if session == nil || session.terminal || session.current != stageIndex || session.record.ContextRevision != revision ||
		session.targetConnectionID != connectionID || session.phase != "offering" {
		return
	}
	now := time.Now().UTC()
	m.completeCurrentStageLocked(session, "decision_timeout", now)
	m.advanceLocked(session)
}

func (m *screeningManager) mediaTimeout(callID string, stageIndex int, revision uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	session := m.sessions[callID]
	if session == nil || session.terminal || session.current != stageIndex || session.record.ContextRevision != revision ||
		(session.phase != "waiting_media" && session.phase != "waiting_active" && session.phase != "controlling") {
		return
	}
	now := time.Now().UTC()
	m.completeCurrentStageLocked(session, "media_timeout", now)
	m.advanceLocked(session)
}

func (m *screeningManager) processingTimeout(callID string, stageIndex int, revision uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	session := m.sessions[callID]
	if session == nil || session.terminal || session.current != stageIndex || session.record.ContextRevision != revision ||
		(session.phase != "controlling" && session.phase != "waiting_media") {
		return
	}
	now := time.Now().UTC()
	m.completeCurrentStageLocked(session, "processing_timeout", now)
	m.advanceLocked(session)
}

func (m *screeningManager) ControllerAttachAllowed(serviceName, connectionID, callID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	session := m.sessions[callID]
	if session == nil || session.terminal {
		return screeningError("screening_not_found", "call has no active screening controller")
	}
	if session.current >= len(session.record.Pipeline) || session.record.Pipeline[session.current] != serviceName {
		return screeningError("wrong_service", "service does not own the current screening stage")
	}
	if session.targetConnectionID != connectionID {
		return screeningError("stale_connection_generation", "controller belongs to a stale service connection")
	}
	if session.phase != "waiting_media" && session.phase != "waiting_active" && session.phase != "controlling" {
		return screeningError("controller_not_reserved", "take_control must be accepted before controller media attaches")
	}
	return nil
}

func (m *screeningManager) MediaState(callID, serviceName, connectionID, sessionID, purpose, state string) {
	if purpose != "controller" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	session := m.sessions[callID]
	if session == nil || session.terminal || session.current >= len(session.record.Pipeline) ||
		session.record.Pipeline[session.current] != serviceName || session.targetConnectionID != connectionID {
		return
	}
	if state == "connected" {
		if session.phase != "waiting_media" && session.phase != "waiting_active" && session.phase != "controlling" {
			return
		}
		m.stopTimer(&session.mediaTimer)
		session.controllerSessionID = sessionID
		if session.record.Answered {
			session.phase = "controlling"
			session.record.State = "controlled"
			m.startProcessingTimerLocked(session)
		} else {
			session.phase = "waiting_active"
			session.record.State = "answering"
			session.carrierAnswerRequested = true
			session.signalInitial("take_control")
		}
		stage := m.currentStageLocked(session)
		stage.Outcome = "controlling"
		m.persistLocked(session)
		return
	}
	if state == "disconnected" && session.controllerSessionID == sessionID && (session.phase == "controlling" || session.phase == "waiting_active") {
		session.controllerSessionID = ""
		session.phase = "waiting_media"
		session.record.State = "waiting_media"
		stage := m.currentStageLocked(session)
		duration := screeningTimeout(stage.MediaTimeoutMS)
		stageIndex, revision := session.current, session.record.ContextRevision
		session.mediaTimer = time.AfterFunc(duration, func() { m.mediaTimeout(callID, stageIndex, revision) })
		m.persistLocked(session)
	}
}

func (m *screeningManager) CallActive(call Call) {
	m.mu.Lock()
	session := m.sessions[call.ID]
	if session == nil {
		m.mu.Unlock()
		m.recoverActive(call)
		return
	}
	if session.terminal {
		m.mu.Unlock()
		return
	}
	session.call = call
	session.record.Answered = true
	if session.phase == "waiting_active" {
		session.phase = "controlling"
		session.record.State = "controlled"
		m.startProcessingTimerLocked(session)
	}
	m.persistLocked(session)
	m.mu.Unlock()
}

func (m *screeningManager) RecoverPersisted() {
	for _, call := range m.store.Calls() {
		if call.Direction == DirectionIncoming && call.EndedAt == nil && call.AnsweredAt != nil &&
			call.Screening != nil && call.Screening.CompletedAt == nil {
			m.recoverActive(call)
		}
	}
}

func (m *screeningManager) recoverActive(call Call) {
	if call.Direction != DirectionIncoming || call.EndedAt != nil || call.AnsweredAt == nil ||
		call.Screening == nil || call.Screening.CompletedAt != nil {
		return
	}
	m.mu.Lock()
	if _, active := m.sessions[call.ID]; active {
		m.mu.Unlock()
		return
	}
	if _, alreadyRecovered := m.recovered[call.ID]; alreadyRecovered {
		m.mu.Unlock()
		return
	}
	call.Screening = cloneCallScreening(call.Screening)
	call.Screening.Answered = true
	call.Screening.State = "ringing_clients"
	call.Screening.Outcome = "ring_clients"
	call.Screening.TerminatedBy = "viti"
	call.Screening.Reason = "manager_restart_fail_open"
	updated, found, err := m.store.UpdateOpenCallScreening(call.ID, call.Screening)
	if err != nil {
		m.events.Publish("screening.persistence.failed", map[string]any{
			"call_id": call.ID, "trace_id": call.TraceID, "error": err.Error(),
		})
	}
	if !found {
		m.mu.Unlock()
		return
	}
	call = updated
	recovery := &screeningRecovery{}
	recovery.timer = time.AfterFunc(m.ringTimeout, func() { m.recoveryExpired(call.ID, recovery) })
	m.recovered[call.ID] = recovery
	m.mu.Unlock()
	if m.callbacks.ringClients != nil {
		go m.runCallAction("restart_fail_open", call, m.callbacks.ringClients)
	}
}

func (m *screeningManager) recoveryExpired(callID string, expected *screeningRecovery) {
	m.mu.Lock()
	recovery := m.recovered[callID]
	if recovery == nil || recovery != expected {
		m.mu.Unlock()
		return
	}
	delete(m.recovered, callID)
	call, found := m.store.GetCall(callID)
	if !found || call.Screening == nil || call.EndedAt != nil {
		m.mu.Unlock()
		return
	}
	now := time.Now().UTC()
	call.Screening = cloneCallScreening(call.Screening)
	call.Screening.State = "ending"
	call.Screening.Outcome = "no_human_answer"
	call.Screening.TerminatedBy = "viti"
	call.Screening.Reason = "ring_timeout"
	call.Screening.CompletedAt = &now
	updated, found, err := m.store.UpdateOpenCallScreening(call.ID, call.Screening)
	if err != nil {
		m.events.Publish("screening.persistence.failed", map[string]any{
			"call_id": call.ID, "trace_id": call.TraceID, "error": err.Error(),
		})
	}
	if !found {
		m.mu.Unlock()
		return
	}
	call = updated
	m.mu.Unlock()
	if m.callbacks.hangup != nil {
		m.runCallAction("restart_ring_timeout_hangup", call, m.callbacks.hangup)
	}
}

func (m *screeningManager) startProcessingTimerLocked(session *screeningSession) {
	stage := m.currentStageLocked(session)
	if stage.ProcessingTimeoutMS == -1 {
		return
	}
	if session.processingTimer != nil {
		return
	}
	if session.processingDeadline.IsZero() {
		session.processingDeadline = time.Now().Add(screeningTimeout(stage.ProcessingTimeoutMS))
	}
	callID, stageIndex, revision := session.call.ID, session.current, session.record.ContextRevision
	remaining := time.Until(session.processingDeadline)
	if remaining < 0 {
		remaining = 0
	}
	session.processingTimer = time.AfterFunc(remaining, func() {
		m.processingTimeout(callID, stageIndex, revision)
	})
}

func screeningTimeout(milliseconds int64) time.Duration {
	const maximumMilliseconds = int64(time.Duration(1<<63-1) / time.Millisecond)
	if milliseconds >= maximumMilliseconds {
		return time.Duration(1<<63 - 1)
	}
	return time.Duration(milliseconds) * time.Millisecond
}

func (m *screeningManager) ServiceDisconnected(serviceName, connectionID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, session := range m.sessions {
		if session.terminal || session.decisionsClosed || session.current >= len(session.record.Pipeline) ||
			session.record.Pipeline[session.current] != serviceName || session.targetConnectionID != connectionID {
			continue
		}
		now := time.Now().UTC()
		m.stopTimer(&session.decisionTimer)
		m.completeCurrentStageLocked(session, "service_disconnected", now)
		m.advanceLocked(session)
	}
}

func (m *screeningManager) HumanJoined(call Call) {
	m.mu.Lock()
	defer m.mu.Unlock()
	session := m.sessions[call.ID]
	if session == nil {
		if recovery := m.recovered[call.ID]; recovery != nil {
			recovery.timer.Stop()
			delete(m.recovered, call.ID)
			stored, found := m.store.GetCall(call.ID)
			if found && stored.Screening != nil {
				now := time.Now().UTC()
				stored.Screening = cloneCallScreening(stored.Screening)
				stored.Screening.State = "human_connected"
				stored.Screening.Outcome = "human_answered"
				stored.Screening.CompletedAt = &now
				m.updateScreeningLocked(stored.ID, stored.TraceID, stored.Screening)
			}
		}
		return
	}
	if session.terminal {
		return
	}
	m.stopTimer(&session.ringTimer)
	now := time.Now().UTC()
	session.decisionsClosed = true
	session.record.State = "human_connected"
	session.record.Outcome = "human_answered"
	session.record.CompletedAt = &now
	m.persistLocked(session)
}

func (m *screeningManager) CallEnded(call Call, actor, reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if recovery := m.recovered[call.ID]; recovery != nil {
		recovery.timer.Stop()
		delete(m.recovered, call.ID)
	}
	session := m.sessions[call.ID]
	if session == nil {
		if latest, found := m.store.GetCall(call.ID); found {
			call = latest
		}
		if call.Screening != nil && call.Screening.CompletedAt == nil {
			now := time.Now().UTC()
			call.Screening = cloneCallScreening(call.Screening)
			call.Screening.State = "ended"
			if call.Screening.Outcome == "" {
				call.Screening.Outcome = "call_ended"
			}
			call.Screening.TerminatedBy = actor
			call.Screening.Reason = reason
			call.Screening.CompletedAt = &now
			m.updateScreeningLocked(call.ID, call.TraceID, call.Screening)
		}
		return
	}
	m.stopAllTimers(session)
	session.signalInitial("hangup")
	now := time.Now().UTC()
	session.terminal = true
	session.decisionsClosed = true
	session.call = call
	session.record.State = "ended"
	if session.record.Outcome == "" {
		session.record.Outcome = "call_ended"
	}
	session.record.TerminatedBy = actor
	session.record.Reason = reason
	session.record.CompletedAt = &now
	m.persistLocked(session)
	delete(m.sessions, call.ID)
}

func (m *screeningManager) finishRingLocked(session *screeningSession, actor, reason string) {
	if session.decisionsClosed || session.terminal {
		return
	}
	session.decisionsClosed = true
	session.phase = "ringing"
	session.record.State = "ringing_clients"
	session.record.Outcome = "ring_clients"
	session.record.TerminatedBy = actor
	session.record.Reason = reason
	m.persistLocked(session)
	if (session.record.Answered || session.carrierAnswerRequested) && m.callbacks.ringClients != nil {
		call := session.call
		go m.runCallAction("ring_clients", call, m.callbacks.ringClients)
	} else {
		session.signalInitial("ring_clients")
	}
	callID := session.call.ID
	session.ringTimer = time.AfterFunc(m.ringTimeout, func() { m.ringExpired(callID) })
}

func (m *screeningManager) ringExpired(callID string) {
	m.mu.Lock()
	session := m.sessions[callID]
	if session == nil || session.terminal || session.record.State == "human_connected" {
		m.mu.Unlock()
		return
	}
	now := time.Now().UTC()
	session.terminal = true
	session.record.State = "ending"
	session.record.Outcome = "no_human_answer"
	session.record.TerminatedBy = "viti"
	session.record.Reason = "ring_timeout"
	session.record.CompletedAt = &now
	m.persistLocked(session)
	call := session.call
	m.mu.Unlock()
	if m.callbacks.hangup != nil {
		m.runCallAction("ring_timeout_hangup", call, m.callbacks.hangup)
	}
}

func (m *screeningManager) finishHangupLocked(session *screeningSession, actor, reason string) {
	if session.terminal {
		return
	}
	m.stopAllTimers(session)
	now := time.Now().UTC()
	session.terminal = true
	session.decisionsClosed = true
	session.record.State = "ending"
	session.record.Outcome = "hangup"
	session.record.TerminatedBy = actor
	session.record.Reason = reason
	session.record.CompletedAt = &now
	m.persistLocked(session)
	if session.record.Answered || session.carrierAnswerRequested {
		if m.callbacks.hangup != nil {
			call := session.call
			go m.runCallAction("screening_hangup", call, m.callbacks.hangup)
		}
	} else {
		session.signalInitial("hangup")
	}
}

func (m *screeningManager) runCallAction(name string, call Call, action func(context.Context, Call) error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := action(ctx, call); err != nil {
		m.events.Publish("screening.action.failed", map[string]any{
			"call_id": call.ID, "trace_id": call.TraceID, "action": name, "error": err.Error(),
		})
	}
}

func (session *screeningSession) signalInitial(action string) {
	session.initialOnce.Do(func() {
		session.initialAction = action
		close(session.initial)
	})
}

func (m *screeningManager) stopTimer(timer **time.Timer) {
	if *timer != nil {
		(*timer).Stop()
		*timer = nil
	}
}

func (m *screeningManager) stopAllTimers(session *screeningSession) {
	m.stopTimer(&session.decisionTimer)
	m.stopTimer(&session.mediaTimer)
	m.stopTimer(&session.processingTimer)
	m.stopTimer(&session.ringTimer)
}

func (m *screeningManager) persistLocked(session *screeningSession) {
	m.updateScreeningLocked(session.call.ID, session.call.TraceID, session.record)
}

func (m *screeningManager) updateScreeningLocked(callID, traceID string, record *CallScreeningRecord) (Call, bool) {
	call, found, err := m.store.UpdateCallScreening(callID, record)
	if err != nil {
		m.events.Publish("screening.persistence.failed", map[string]any{
			"call_id": callID, "trace_id": traceID, "error": err.Error(),
		})
	}
	if !found {
		m.events.Publish("screening.persistence.failed", map[string]any{
			"call_id": callID, "trace_id": traceID, "error": "call record is missing",
		})
	}
	return call, found
}

func (r *integrationRegistry) screeningService(name string) (*integrationConnection, screeningCapabilityOptions, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	connection := r.connections[name]
	if connection == nil {
		return nil, screeningCapabilityOptions{}.normalized(), false
	}
	raw, granted := connection.capabilities["call.screen"]
	if !granted {
		return nil, screeningCapabilityOptions{}.normalized(), false
	}
	options := screeningCapabilityOptions{}
	if len(bytes.TrimSpace(raw)) != 0 {
		if err := json.Unmarshal(raw, &options); err != nil || options.validate() != nil {
			return nil, screeningCapabilityOptions{}.normalized(), false
		}
	}
	return connection, options.normalized(), true
}

func (r *integrationRegistry) connectionHasCapability(name, connectionID, capability string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	connection := r.connections[name]
	if connection == nil || connection.id != connectionID {
		return false
	}
	_, granted := connection.capabilities[capability]
	return granted
}
