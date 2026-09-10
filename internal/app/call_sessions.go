package app

import (
	"errors"
	"strings"
	"sync"
	"time"
)

var (
	errCallParticipantNotFound = errors.New("call participant not found")
	errLeaveOperationConflict  = errors.New("leave operation was already used for another participant")
)

// callSessionRegistry is the manager's volatile control-plane view of local
// conference membership. The carrier channel remains the authority for whether
// a call exists; this registry only remembers user intent across replacement
// WebRTC legs. Manager/engine restart survival is deliberately outside issue #11.
type callSessionRegistry struct {
	mu          sync.Mutex
	sessions    map[string]*callSession
	operations  map[string]*callLeaveOperation
	claims      map[string]string
	endedTraces map[string]time.Time
}

type callSession struct {
	instance     string
	engineCallID string
	traceID      string
	participants map[string]callParticipant
	departed     map[string]callParticipantDeparture
}

type callParticipant struct {
	clientID      string
	participantID string
	connectionID  string
	channel       string
	connected     bool
	updatedAt     time.Time
}

type callParticipantDeparture struct {
	connectionID string
	at           time.Time
}

type callParticipantJoinResult struct {
	supersededChannel string
	rejectedChannel   string
}

type callLeaveOperation struct {
	operationID  string
	callKey      string
	traceID      string
	clientID     string
	connectionID string
	channel      string
	endCarrier   bool
	completed    bool
}

func newCallSessionRegistry() *callSessionRegistry {
	return &callSessionRegistry{
		sessions:    make(map[string]*callSession),
		operations:  make(map[string]*callLeaveOperation),
		claims:      make(map[string]string),
		endedTraces: make(map[string]time.Time),
	}
}

func callSessionKey(instance, engineCallID string) string { return instance + "\x00" + engineCallID }

func logicalParticipantKey(clientID, participantID string) string {
	if clientID != "" {
		return "client:" + clientID
	}
	return "participant:" + participantID
}

func participantClaimKey(traceID, participantID string) string {
	return traceID + "\x00" + participantID
}

// joined returns the superseded channel, if the same logical client already
// had another live connection. Events from an older connection generation are
// harmless because left() also matches connectionID.
func (r *callSessionRegistry) joined(call Call, clientID, participantID, connectionID, channel string, at time.Time) callParticipantJoinResult {
	r.mu.Lock()
	defer r.mu.Unlock()
	if clientID == "" {
		clientID = r.claims[participantClaimKey(call.TraceID, participantID)]
	}
	key := callSessionKey(call.Instance, call.EngineCallID)
	session := r.sessions[key]
	if session == nil {
		session = &callSession{
			instance: call.Instance, engineCallID: call.EngineCallID, traceID: call.TraceID,
			participants: make(map[string]callParticipant), departed: make(map[string]callParticipantDeparture),
		}
		r.sessions[key] = session
	}
	logicalKey := logicalParticipantKey(clientID, participantID)
	if departure, found := session.departed[logicalKey]; found {
		// A delayed duplicate of the explicitly departed connection can never
		// rejoin. A genuinely new connection may join later, but only when its
		// engine timestamp is newer than the departure tombstone.
		if departure.connectionID == connectionID || !at.After(departure.at) {
			return callParticipantJoinResult{rejectedChannel: channel}
		}
		delete(session.departed, logicalKey)
	}
	previous, found := session.participants[logicalKey]
	if found && !at.After(previous.updatedAt) {
		if previous.connectionID != connectionID || previous.channel != channel {
			return callParticipantJoinResult{rejectedChannel: channel}
		}
		return callParticipantJoinResult{}
	}
	if found && previous.connected && previous.connectionID == connectionID && previous.channel != channel {
		// A connection generation identifies exactly one Asterisk channel.
		// Reusing it for a second channel would make future leave operations
		// ambiguous, so retain the first channel and reject the duplicate.
		return callParticipantJoinResult{rejectedChannel: channel}
	}
	superseded := ""
	if found && previous.connected && previous.connectionID != connectionID {
		superseded = previous.channel
	}
	session.participants[logicalKey] = callParticipant{
		clientID: clientID, participantID: participantID, connectionID: connectionID,
		channel: channel, connected: true, updatedAt: at,
	}
	return callParticipantJoinResult{supersededChannel: superseded}
}

func (r *callSessionRegistry) left(call Call, clientID, participantID, connectionID string, at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	session := r.sessions[callSessionKey(call.Instance, call.EngineCallID)]
	if session == nil {
		return
	}
	logicalKey := logicalParticipantKey(clientID, participantID)
	participant, found := session.participants[logicalKey]
	if !found || participant.connectionID != connectionID || at.Before(participant.updatedAt) {
		return
	}
	participant.connected = false
	participant.channel = ""
	participant.updatedAt = at
	session.participants[logicalKey] = participant
}

// claim replaces the provisional Asterisk participant identity used for an
// inbound fork with the stable device identity reported by the answered client.
func (r *callSessionRegistry) claim(traceID, participantID, clientID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ended := r.endedTraces[traceID]; ended {
		return false
	}
	r.claims[participantClaimKey(traceID, participantID)] = clientID
	for _, session := range r.sessions {
		if session.traceID != traceID {
			continue
		}
		provisionalKey := logicalParticipantKey("", participantID)
		participant, found := session.participants[provisionalKey]
		if !found {
			return true
		}
		delete(session.participants, provisionalKey)
		participant.clientID = clientID
		stableKey := logicalParticipantKey(clientID, participantID)
		if existing, ok := session.participants[stableKey]; ok && existing.updatedAt.After(participant.updatedAt) {
			return true
		}
		session.participants[stableKey] = participant
		return true
	}
	return true
}

// beginLeave removes logical membership before any external AMI command. The
// retained operation makes a retry after a lost HTTP response converge on the
// same directed action rather than acting on a later call or connection.
func (r *callSessionRegistry) beginLeave(call Call, clientID, connectionID, operationID string) (callLeaveOperation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := callSessionKey(call.Instance, call.EngineCallID)
	if existing := r.operations[operationID]; existing != nil {
		if existing.callKey != key || existing.clientID != clientID || existing.connectionID != connectionID {
			return callLeaveOperation{}, errLeaveOperationConflict
		}
		return *existing, nil
	}
	session := r.sessions[key]
	if session == nil {
		return callLeaveOperation{}, errCallParticipantNotFound
	}
	logicalKey := logicalParticipantKey(clientID, "")
	participant, found := session.participants[logicalKey]
	if !found || participant.connectionID != connectionID {
		return callLeaveOperation{}, errCallParticipantNotFound
	}
	delete(session.participants, logicalKey)
	session.departed[logicalKey] = callParticipantDeparture{connectionID: connectionID, at: time.Now().UTC()}
	operation := &callLeaveOperation{
		operationID: operationID, callKey: key, traceID: call.TraceID,
		clientID: clientID, connectionID: connectionID, channel: participant.channel,
		endCarrier: len(session.participants) == 0,
	}
	r.operations[operationID] = operation
	return *operation, nil
}

func (r *callSessionRegistry) completeLeave(operationID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if operation := r.operations[operationID]; operation != nil {
		operation.completed = true
	}
}

func (r *callSessionRegistry) ended(instance, engineCallID, traceID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := callSessionKey(instance, engineCallID)
	session := r.sessions[key]
	delete(r.sessions, key)
	if session != nil && traceID == "" {
		traceID = session.traceID
	}
	if traceID != "" {
		r.endedTraces[traceID] = time.Now().UTC()
		if len(r.endedTraces) > maxRememberedEndedTraces {
			oldestTrace := ""
			var oldestAt time.Time
			for candidate, endedAt := range r.endedTraces {
				if oldestTrace == "" || endedAt.Before(oldestAt) {
					oldestTrace, oldestAt = candidate, endedAt
				}
			}
			delete(r.endedTraces, oldestTrace)
		}
		prefix := traceID + "\x00"
		for claimKey := range r.claims {
			if strings.HasPrefix(claimKey, prefix) {
				delete(r.claims, claimKey)
			}
		}
	}
	for operationID, operation := range r.operations {
		if operation.callKey == key {
			delete(r.operations, operationID)
		}
	}
}

const maxRememberedEndedTraces = 1024

func validCallControlID(value string) bool {
	value = strings.TrimSpace(value)
	return value != "" && len(value) <= 160 && topologyIdentifierPattern.MatchString(value)
}
