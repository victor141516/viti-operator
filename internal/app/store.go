package app

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"
)

type diskState struct {
	Messages               []Message                         `json:"messages"`
	Calls                  []Call                            `json:"calls"`
	Clients                []KnownClient                     `json:"clients,omitempty"`
	SIMAuthAssignment      SIMAuthAssignment                 `json:"sim_auth_assignment,omitempty"`
	IMSRegistrationEnabled *bool                             `json:"ims_registration_enabled,omitempty"`
	CallRecordingEnabled   *bool                             `json:"call_recording_enabled,omitempty"`
	Contacts               []ContactSourceRecord             `json:"contacts,omitempty"`
	ContactSourceRevisions map[string]int64                  `json:"contact_source_revisions,omitempty"`
	ContactsVersion        uint64                            `json:"contacts_version,omitempty"`
	EngineTunnels          map[string]EngineTunnelCheckpoint `json:"engine_tunnels,omitempty"`
}

type Store struct {
	mu                     sync.RWMutex
	path                   string
	messages               map[string]Message
	calls                  map[string]Call
	clients                map[string]KnownClient
	simAuthAssignment      SIMAuthAssignment
	imsRegistrationEnabled bool
	callRecordingEnabled   bool
	contacts               map[string]ContactSourceRecord
	contactSourceRevisions map[string]int64
	contactView            []Contact
	contactsVersion        uint64
	engineTunnels          map[string]EngineTunnelCheckpoint
	diag                   *DiagnosticRecorder
}

const clientLastSeenPersistInterval = time.Minute

func (s *Store) SetDiagnostics(diagnostics *DiagnosticRecorder) {
	s.diag = diagnostics
}

func OpenStore(path string) (*Store, error) {
	s := &Store{path: path, messages: map[string]Message{}, calls: map[string]Call{}, clients: map[string]KnownClient{}, contacts: map[string]ContactSourceRecord{}, contactSourceRevisions: map[string]int64{}, engineTunnels: map[string]EngineTunnelCheckpoint{}, imsRegistrationEnabled: true}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return s, nil
		}
		return nil, err
	}
	var state diskState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, err
	}
	normalizedState := false
	for _, item := range state.Messages {
		normalized := normalizeStoredMessage(item, time.Now().UTC())
		s.messages[item.ID] = normalized
		normalizedState = normalizedState || !reflect.DeepEqual(item, normalized)
	}
	for _, item := range state.Calls {
		s.calls[item.ID] = item
	}
	for _, item := range state.Clients {
		s.clients[item.ID] = item
	}
	for _, item := range state.Contacts {
		s.contacts[contactSourceKey(item.SourceClientID, item.SourceContactID)] = item
	}
	for clientID, revision := range state.ContactSourceRevisions {
		s.contactSourceRevisions[clientID] = revision
	}
	s.contactView = mergedContacts(contactSourcesFromMap(s.contacts), "")
	s.contactsVersion = state.ContactsVersion
	for instance, checkpoint := range state.EngineTunnels {
		checkpoint.RetiredGenerations = append([]string(nil), checkpoint.RetiredGenerations...)
		s.engineTunnels[instance] = checkpoint
	}
	s.simAuthAssignment = state.SIMAuthAssignment
	if state.IMSRegistrationEnabled != nil {
		s.imsRegistrationEnabled = *state.IMSRegistrationEnabled
	}
	if state.CallRecordingEnabled != nil {
		s.callRecordingEnabled = *state.CallRecordingEnabled
	}
	if normalizedState {
		if err := s.flushLocked(); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *Store) CallRecordingEnabled() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.callRecordingEnabled
}

func (s *Store) SetCallRecordingEnabled(enabled bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.callRecordingEnabled == enabled {
		return nil
	}
	s.callRecordingEnabled = enabled
	return s.flushLocked()
}

func (s *Store) RememberClient(client KnownClient) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if previous, ok := s.clients[client.ID]; ok {
		sameIdentity := previous.ID == client.ID && previous.Kind == client.Kind && previous.Name == client.Name &&
			previous.AuthAgentID == client.AuthAgentID && previous.HasSIM == client.HasSIM
		if sameIdentity && !previous.LastSeen.IsZero() && client.LastSeen.Sub(previous.LastSeen) < clientLastSeenPersistInterval {
			return nil
		}
	}
	s.clients[client.ID] = client
	return s.flushLocked()
}

func (s *Store) KnownClients() []KnownClient {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]KnownClient, 0, len(s.clients))
	for _, client := range s.clients {
		result = append(result, client)
	}
	return result
}

func contactSourceKey(clientID, contactID string) string { return clientID + "\x00" + contactID }

func (s *Store) ReplaceContactSource(clientID, clientName, region string, revision int64, records []ContactSourceRecord) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	currentRevision := s.contactSourceRevisions[clientID]
	if currentRevision > 0 && revision <= currentRevision {
		return false, nil
	}
	proposed := make(map[string]ContactSourceRecord, len(records))
	for _, record := range records {
		record.SourceClientID = clientID
		record.SourceClientName = clientName
		record.Region = region
		record.UpdatedAt = time.Time{}
		proposed[contactSourceKey(clientID, record.SourceContactID)] = record
	}
	existing := make(map[string]ContactSourceRecord, len(proposed))
	for key, record := range s.contacts {
		if record.SourceClientID != clientID {
			continue
		}
		record.UpdatedAt = time.Time{}
		existing[key] = record
	}
	contentChanged := !reflect.DeepEqual(existing, proposed)
	if !contentChanged && revision <= 0 {
		return false, nil
	}
	nextContacts := make(map[string]ContactSourceRecord, len(s.contacts)-len(existing)+len(records))
	for key, record := range s.contacts {
		if contentChanged && record.SourceClientID == clientID {
			continue
		}
		nextContacts[key] = record
	}
	if contentChanged {
		now := time.Now().UTC()
		for _, record := range records {
			record.SourceClientID = clientID
			record.SourceClientName = clientName
			record.Region = region
			record.UpdatedAt = now
			nextContacts[contactSourceKey(clientID, record.SourceContactID)] = record
		}
	}
	nextRevisions := make(map[string]int64, len(s.contactSourceRevisions)+1)
	for id, value := range s.contactSourceRevisions {
		nextRevisions[id] = value
	}
	if revision > 0 {
		nextRevisions[clientID] = revision
	}
	previousContacts, previousRevisions, previousView, previousVersion := s.contacts, s.contactSourceRevisions, s.contactView, s.contactsVersion
	s.contacts = nextContacts
	s.contactSourceRevisions = nextRevisions
	if contentChanged {
		s.contactView = mergedContacts(contactSourcesFromMap(nextContacts), "")
		s.contactsVersion++
	}
	if err := s.flushLocked(); err != nil {
		s.contacts, s.contactSourceRevisions, s.contactView, s.contactsVersion = previousContacts, previousRevisions, previousView, previousVersion
		return false, err
	}
	return contentChanged, nil
}

func contactSourcesFromMap(records map[string]ContactSourceRecord) []ContactSourceRecord {
	result := make([]ContactSourceRecord, 0, len(records))
	for _, record := range records {
		record.Names = append([]string(nil), record.Names...)
		record.Numbers = append([]ContactNumber(nil), record.Numbers...)
		result = append(result, record)
	}
	return result
}

func (s *Store) ContactSources() []ContactSourceRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return contactSourcesFromMap(s.contacts)
}

func (s *Store) Contacts(query string) []Contact {
	contacts, _ := s.ContactsSnapshot(query)
	return contacts
}

func (s *Store) ContactsSnapshot(query string) ([]Contact, uint64) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]Contact, 0, len(s.contactView))
	for _, contact := range s.contactView {
		if !contactMatches(contact, query) {
			continue
		}
		contact.Names = append([]string(nil), contact.Names...)
		contact.Numbers = append([]ContactNumber(nil), contact.Numbers...)
		contact.Sources = append([]ContactSource(nil), contact.Sources...)
		result = append(result, contact)
	}
	return result, s.contactsVersion
}

func (s *Store) ContactsVersion() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.contactsVersion
}

func (s *Store) SIMAuthAssignment() SIMAuthAssignment {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.simAuthAssignment
}

func (s *Store) SetSIMAuthAssignment(assignment SIMAuthAssignment) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.simAuthAssignment == assignment {
		return nil
	}
	s.simAuthAssignment = assignment
	return s.flushLocked()
}

func (s *Store) IMSRegistrationEnabled() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.imsRegistrationEnabled
}

func (s *Store) SetIMSRegistrationEnabled(enabled bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.imsRegistrationEnabled = enabled
	return s.flushLocked()
}

func (s *Store) SaveMessage(message Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	previous, existed := s.messages[message.ID]
	s.messages[message.ID] = cloneMessage(message)
	if err := s.flushLocked(); err != nil {
		if existed {
			s.messages[message.ID] = previous
		} else {
			delete(s.messages, message.ID)
		}
		return err
	}
	return nil
}

func (s *Store) GetMessage(id string) (Message, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	message, ok := s.messages[id]
	return cloneMessage(message), ok
}

func (s *Store) SaveCall(call Call) error {
	s.mu.Lock()
	previous, existed := s.calls[call.ID]
	s.calls[call.ID] = call
	err := s.flushLocked()
	if err != nil {
		if existed {
			s.calls[call.ID] = previous
		} else {
			delete(s.calls, call.ID)
		}
	}
	s.mu.Unlock()
	s.diag.RecordTrace("manager.store", "call.saved", call.TraceID, map[string]any{
		"existed": existed, "previous": previous, "current": call,
		"path": s.path, "persist_error": errorString(err),
	})
	return err
}

type engineCallUpdate struct {
	ProposedID   string
	Instance     string
	EngineCallID string
	TraceID      string
	Event        string
	Direction    Direction
	Peer         string
	At           time.Time
	DialStatus   string
	HangupCause  string
}

var errEngineCallAwaitingStart = errors.New("engine call start has not arrived")

func engineTunnelDownBoundary(checkpoint EngineTunnelCheckpoint) time.Time {
	if !checkpoint.LastDownAt.IsZero() {
		return checkpoint.LastDownAt
	}
	// Compatibility with checkpoints written before LastDownAt existed. For a
	// down record UpdatedAt is the down boundary; for an up record it is a
	// conservative generation boundary that rejects only older hooks.
	return checkpoint.UpdatedAt
}

// ApplyEngineCallEvent performs lookup, lifecycle mutation, tunnel
// reconciliation and persistence under one lock. Engine hooks are independent
// processes, so composing GetCall and SaveCall in the HTTP handler would allow
// duplicate rows, state regression, or resurrection across a concurrent down.
func (s *Store) ApplyEngineCallEvent(update engineCallUpdate) (Call, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var call Call
	found := false
	if update.EngineCallID != "" {
		for _, candidate := range s.calls {
			if candidate.Instance == update.Instance && candidate.EngineCallID == update.EngineCallID {
				call, found = candidate, true
				break
			}
		}
	} else {
		for _, candidate := range s.calls {
			if candidate.EndedAt == nil && candidate.Direction == update.Direction && (update.Peer == "" || candidate.Peer == update.Peer) {
				call, found = candidate, true
				break
			}
		}
	}

	if found && call.EndedAt != nil {
		// A start hook may itself have been delayed beyond the terminal event.
		// It may refine the immutable start boundary, but must never resurrect or
		// otherwise rewrite the completed lifecycle.
		if (update.Event != "call_in" && update.Event != "call_out") ||
			(!call.StartedAt.IsZero() && !update.At.Before(call.StartedAt)) {
			return call, false, nil
		}
		previous := call
		call.StartedAt = update.At
		if call.State == "interrupted" {
			if checkpoint, ok := s.engineTunnels[update.Instance]; ok {
				downAt := engineTunnelDownBoundary(checkpoint)
				if !downAt.IsZero() && !downAt.Before(call.StartedAt) && call.EndedAt != nil && downAt.Before(*call.EndedAt) {
					endedAt := downAt
					call.UpdatedAt = endedAt
					call.EndedAt = &endedAt
					if call.AnsweredAt != nil && call.AnsweredAt.After(endedAt) {
						call.AnsweredAt = nil
					}
				}
			}
		}
		s.calls[call.ID] = call
		if err := s.flushLocked(); err != nil {
			s.calls[call.ID] = previous
			return previous, false, err
		}
		return call, true, nil
	}
	if !found {
		if update.Peer == "" {
			return Call{}, false, errEngineCallAwaitingStart
		}
		call = Call{
			ID: update.ProposedID, TraceID: update.TraceID, EngineCallID: update.EngineCallID,
			Instance: update.Instance, Direction: update.Direction, Peer: update.Peer,
			StartedAt: update.At,
		}
	}
	previous, existed := call, found
	if call.TraceID == "" {
		call.TraceID = update.TraceID
	}
	if call.StartedAt.IsZero() || update.At.Before(call.StartedAt) {
		call.StartedAt = update.At
	}

	switch update.Event {
	case "call_in", "call_out":
		if call.State != "active" {
			call.State = map[Direction]string{DirectionIncoming: "ringing", DirectionOutgoing: "dialing"}[update.Direction]
		}
		if call.UpdatedAt.IsZero() || update.At.After(call.UpdatedAt) {
			call.UpdatedAt = update.At
		}
	case "call_active":
		call.State = "active"
		if call.AnsweredAt == nil || update.At.Before(*call.AnsweredAt) {
			answeredAt := update.At
			call.AnsweredAt = &answeredAt
		}
		if call.UpdatedAt.IsZero() || update.At.After(call.UpdatedAt) {
			call.UpdatedAt = update.At
		}
	case "call_result":
		call.State = callResultState(update.DialStatus, call.State, update.HangupCause)
		if call.AnsweredAt != nil && call.AnsweredAt.After(update.At) {
			call.AnsweredAt = nil
		}
		if call.UpdatedAt.IsZero() || update.At.After(call.UpdatedAt) {
			call.UpdatedAt = update.At
		}
		endedAt := update.At
		call.EndedAt = &endedAt
	}

	// A durable down may overtake an older best-effort call webhook. Reconcile
	// the call in the same critical section so either ordering produces the same
	// terminal state and a concurrent handler cannot resurrect it afterward.
	if call.EndedAt == nil {
		if checkpoint, ok := s.engineTunnels[update.Instance]; ok {
			downAt := engineTunnelDownBoundary(checkpoint)
			if !downAt.IsZero() && !downAt.Before(call.StartedAt) {
				call.State = "interrupted"
				endedAt := downAt
				if call.AnsweredAt != nil && call.AnsweredAt.After(endedAt) {
					call.AnsweredAt = nil
				}
				call.UpdatedAt = endedAt
				call.EndedAt = &endedAt
			}
		}
	}

	s.calls[call.ID] = call
	if err := s.flushLocked(); err != nil {
		if existed {
			s.calls[call.ID] = previous
		} else {
			delete(s.calls, call.ID)
		}
		return previous, false, err
	}
	return call, true, nil
}

// ReconcilePersistedCalls removes only records that are structurally invalid.
// Age alone is not evidence that an established carrier call ended: Asterisk
// and its media session can remain live while the manager restarts.
func (s *Store) ReconcilePersistedCalls() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	for id, call := range s.calls {
		// An outgoing call can never be valid without a destination. Old engine
		// hooks could manufacture such rows from an unmatched hangup event.
		if call.Direction == DirectionOutgoing && strings.TrimSpace(call.Peer) == "" {
			s.diag.RecordTrace("manager.store", "call.reconcile.delete-invalid", call.TraceID, call)
			delete(s.calls, id)
			changed = true
			continue
		}
	}
	if !changed {
		return nil
	}
	return s.flushLocked()
}

func (s *Store) InterruptOpenCalls(at time.Time) ([]Call, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := make([]Call, 0)
	for id, call := range s.calls {
		if call.EndedAt != nil {
			continue
		}
		previous := call
		call.State = "interrupted"
		call.UpdatedAt = at
		call.EndedAt = &at
		s.calls[id] = call
		changed = append(changed, call)
		s.diag.RecordTrace("manager.store", "call.interrupted-runtime-down", call.TraceID, map[string]any{"previous": previous, "current": call})
	}
	if len(changed) == 0 {
		return nil, nil
	}
	if err := s.flushLocked(); err != nil {
		return nil, err
	}
	return changed, nil
}

// ApplyEngineTunnelEvent atomically advances the durable event cursor and
// interrupts open call records only for an accepted up-to-down transition.
// Persisting both effects together makes a retried event idempotent even when
// the manager loses its HTTP response or restarts.
func (s *Store) ApplyEngineTunnelEvent(instance, generation string, sequence uint64, ready, claimsGeneration bool, at time.Time) (EngineTunnelCheckpoint, []Call, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	previous := s.engineTunnels[instance]
	next, accepted := advanceEngineTunnelCheckpoint(previous, instance, generation, sequence, ready, claimsGeneration, at)
	if !accepted {
		return previous, nil, false, nil
	}
	previousCalls := make(map[string]Call)
	changedCalls := make([]Call, 0)
	if !next.Ready && (previous.Ready || next.LastDownAt.After(previous.LastDownAt)) {
		for id, call := range s.calls {
			if call.EndedAt != nil || (!call.StartedAt.IsZero() && call.StartedAt.After(at)) {
				continue
			}
			previousCalls[id] = call
			call.State = "interrupted"
			if call.AnsweredAt != nil && call.AnsweredAt.After(at) {
				call.AnsweredAt = nil
			}
			call.UpdatedAt = at
			call.EndedAt = &at
			s.calls[id] = call
			changedCalls = append(changedCalls, call)
		}
	}
	s.engineTunnels[instance] = next
	if err := s.flushLocked(); err != nil {
		if previous.Generation == "" {
			delete(s.engineTunnels, instance)
		} else {
			s.engineTunnels[instance] = previous
		}
		for id, call := range previousCalls {
			s.calls[id] = call
		}
		return previous, nil, false, err
	}
	return next, changedCalls, true, nil
}

func (s *Store) LatestEngineTunnelCheckpoint() (EngineTunnelCheckpoint, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var latest EngineTunnelCheckpoint
	found := false
	for _, checkpoint := range s.engineTunnels {
		if !found || checkpoint.UpdatedAt.After(latest.UpdatedAt) {
			latest = checkpoint
			found = true
		}
	}
	latest.RetiredGenerations = append([]string(nil), latest.RetiredGenerations...)
	return latest, found
}

func (s *Store) EngineTunnelCheckpoint(instance string) (EngineTunnelCheckpoint, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	checkpoint, found := s.engineTunnels[instance]
	checkpoint.RetiredGenerations = append([]string(nil), checkpoint.RetiredGenerations...)
	return checkpoint, found
}

func (s *Store) GetCall(id string) (Call, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	call, ok := s.calls[id]
	return call, ok
}

func (s *Store) GetCallByEngineID(instance, engineCallID string) (Call, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, call := range s.calls {
		if call.EngineCallID == engineCallID && call.Instance == instance {
			return call, true
		}
	}
	return Call{}, false
}

func (s *Store) GetCallByTraceID(traceID string) (Call, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, call := range s.calls {
		if call.TraceID == traceID {
			return call, true
		}
	}
	return Call{}, false
}

func (s *Store) ClaimRecordingParticipant(traceID string, participant RecordingParticipant) (Call, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, call := range s.calls {
		if call.TraceID != traceID {
			continue
		}
		updated := false
		for index, existing := range call.RecordingParticipants {
			if existing.ParticipantID == participant.ParticipantID {
				call.RecordingParticipants[index] = participant
				updated = true
				break
			}
		}
		if !updated {
			call.RecordingParticipants = append(call.RecordingParticipants, participant)
		}
		for index, track := range call.RecordingTracks {
			if track.ParticipantID == participant.ParticipantID {
				track.ClientID = participant.ClientID
				track.Label = participant.Name
				call.RecordingTracks[index] = track
			}
		}
		s.calls[id] = call
		return call, true, s.flushLocked()
	}
	return Call{}, false, nil
}

func (s *Store) AddRecordingTrack(instance, engineCallID string, track RecordingTrack) (Call, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, call := range s.calls {
		if call.Instance != instance || call.EngineCallID != engineCallID {
			continue
		}
		for _, participant := range call.RecordingParticipants {
			if participant.ParticipantID == track.ParticipantID {
				track.ClientID = participant.ClientID
				track.Label = participant.Name
				break
			}
		}
		replaced := false
		for index := range call.RecordingTracks {
			if call.RecordingTracks[index].ID == track.ID {
				call.RecordingTracks[index] = track
				replaced = true
				break
			}
		}
		if !replaced {
			call.RecordingTracks = append(call.RecordingTracks, track)
		}
		s.calls[id] = call
		return call, true, s.flushLocked()
	}
	return Call{}, false, nil
}

// UpdateRecordingTrackTiming changes only derived timing fields under the
// store lock. It deliberately does not replace the surrounding Call snapshot,
// because recording and participant events may arrive concurrently.
func (s *Store) UpdateRecordingTrackTiming(callID, trackID string, offsetMS, durationMS int64) (Call, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	call, found := s.calls[callID]
	if !found {
		return Call{}, false, nil
	}
	for index := range call.RecordingTracks {
		track := &call.RecordingTracks[index]
		if track.ID != trackID {
			continue
		}
		if track.OffsetMS == offsetMS && track.DurationMS == durationMS {
			return call, true, nil
		}
		track.OffsetMS = offsetMS
		track.DurationMS = durationMS
		s.calls[callID] = call
		return call, true, s.flushLocked()
	}
	return call, false, nil
}

func (s *Store) Messages() []Message {
	s.mu.RLock()
	defer s.mu.RUnlock()
	items := make([]Message, 0, len(s.messages))
	for _, item := range s.messages {
		items = append(items, cloneMessage(item))
	}
	sort.Slice(items, func(i, j int) bool { return items[i].CreatedAt.After(items[j].CreatedAt) })
	return items
}

func (s *Store) Calls() []Call {
	s.mu.RLock()
	defer s.mu.RUnlock()
	items := make([]Call, 0, len(s.calls))
	for _, item := range s.calls {
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].StartedAt.After(items[j].StartedAt) })
	return items
}

func (s *Store) flushLocked() error {
	enabled := s.imsRegistrationEnabled
	recordingEnabled := s.callRecordingEnabled
	state := diskState{IMSRegistrationEnabled: &enabled, CallRecordingEnabled: &recordingEnabled, SIMAuthAssignment: s.simAuthAssignment}
	for _, item := range s.messages {
		state.Messages = append(state.Messages, item)
	}
	for _, item := range s.calls {
		state.Calls = append(state.Calls, item)
	}
	for _, item := range s.clients {
		state.Clients = append(state.Clients, item)
	}
	for _, item := range s.contacts {
		state.Contacts = append(state.Contacts, item)
	}
	state.ContactSourceRevisions = s.contactSourceRevisions
	state.ContactsVersion = s.contactsVersion
	if len(s.engineTunnels) > 0 {
		state.EngineTunnels = make(map[string]EngineTunnelCheckpoint, len(s.engineTunnels))
		for instance, checkpoint := range s.engineTunnels {
			checkpoint.RetiredGenerations = append([]string(nil), checkpoint.RetiredGenerations...)
			state.EngineTunnels[instance] = checkpoint
		}
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
