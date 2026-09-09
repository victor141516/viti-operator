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
	Messages               []Message             `json:"messages"`
	Calls                  []Call                `json:"calls"`
	Clients                []KnownClient         `json:"clients,omitempty"`
	SIMAuthAssignment      SIMAuthAssignment     `json:"sim_auth_assignment,omitempty"`
	IMSRegistrationEnabled *bool                 `json:"ims_registration_enabled,omitempty"`
	CallRecordingEnabled   *bool                 `json:"call_recording_enabled,omitempty"`
	Contacts               []ContactSourceRecord `json:"contacts,omitempty"`
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
	diag                   *DiagnosticRecorder
}

const clientLastSeenPersistInterval = time.Minute

func (s *Store) SetDiagnostics(diagnostics *DiagnosticRecorder) {
	s.diag = diagnostics
}

func OpenStore(path string) (*Store, error) {
	s := &Store{path: path, messages: map[string]Message{}, calls: map[string]Call{}, clients: map[string]KnownClient{}, contacts: map[string]ContactSourceRecord{}, imsRegistrationEnabled: true}
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
	for _, item := range state.Messages {
		s.messages[item.ID] = item
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
	s.simAuthAssignment = state.SIMAuthAssignment
	if state.IMSRegistrationEnabled != nil {
		s.imsRegistrationEnabled = *state.IMSRegistrationEnabled
	}
	if state.CallRecordingEnabled != nil {
		s.callRecordingEnabled = *state.CallRecordingEnabled
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

func (s *Store) ReplaceContactSource(clientID, clientName, region string, records []ContactSourceRecord) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
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
	if reflect.DeepEqual(existing, proposed) {
		return false, nil
	}
	for key, record := range s.contacts {
		if record.SourceClientID == clientID {
			delete(s.contacts, key)
		}
	}
	now := time.Now().UTC()
	for _, record := range records {
		record.SourceClientID = clientID
		record.SourceClientName = clientName
		record.Region = region
		record.UpdatedAt = now
		s.contacts[contactSourceKey(clientID, record.SourceContactID)] = record
	}
	return true, s.flushLocked()
}

func (s *Store) ContactSources() []ContactSourceRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]ContactSourceRecord, 0, len(s.contacts))
	for _, record := range s.contacts {
		record.Names = append([]string(nil), record.Names...)
		record.Numbers = append([]ContactNumber(nil), record.Numbers...)
		result = append(result, record)
	}
	return result
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
	s.messages[message.ID] = message
	return s.flushLocked()
}

func (s *Store) GetMessage(id string) (Message, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	message, ok := s.messages[id]
	return message, ok
}

func (s *Store) SaveCall(call Call) error {
	s.mu.Lock()
	previous, existed := s.calls[call.ID]
	s.calls[call.ID] = call
	err := s.flushLocked()
	s.mu.Unlock()
	s.diag.RecordTrace("manager.store", "call.saved", call.TraceID, map[string]any{
		"existed": existed, "previous": previous, "current": call,
		"path": s.path, "persist_error": errorString(err),
	})
	return err
}

// CloseStaleCalls releases the global line after an unclean engine/server
// restart. Open records are advisory state; they must never block every client
// forever when their final webhook was missed.
func (s *Store) CloseStaleCalls(before time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	now := time.Now().UTC()
	for id, call := range s.calls {
		// An outgoing call can never be valid without a destination. Old engine
		// hooks could manufacture such rows from an unmatched hangup event.
		if call.Direction == DirectionOutgoing && strings.TrimSpace(call.Peer) == "" {
			s.diag.RecordTrace("manager.store", "call.reconcile.delete-invalid", call.TraceID, call)
			delete(s.calls, id)
			changed = true
			continue
		}
		if call.EndedAt == nil && call.UpdatedAt.Before(before) {
			previous := call
			call.State = "interrupted"
			call.UpdatedAt = now
			call.EndedAt = &now
			s.calls[id] = call
			s.diag.RecordTrace("manager.store", "call.reconcile.interrupted", call.TraceID, map[string]any{"previous": previous, "current": call})
			changed = true
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
		items = append(items, item)
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
