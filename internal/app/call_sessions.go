package app

import (
	"errors"
	"sort"
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
	instance           string
	engineCallID       string
	traceID            string
	participants       map[string]callParticipant
	departed           map[string]callParticipantDeparture
	pendingDrops       map[string]callParticipant
	closingOperationID string
	explicitLeaveSeen  bool
}

type callParticipant struct {
	clientID         string
	participantID    string
	connectionID     string
	channel          string
	admissionChannel string
	connected        bool
	logicalAnnounced bool
	mediaAnnounced   bool
	updatedAt        time.Time
	predecessor      *callParticipant
}

type callParticipantDeparture struct {
	connectionIDs        map[string]struct{}
	abortedConnectionIDs map[string]struct{}
	at                   time.Time
}

type callParticipantJoinResult struct {
	channelsToDrop []string
	admitted       bool
}

type callParticipantAbortResult struct {
	participant       callParticipant
	logicalRemoved    bool
	mediaDisconnected bool
	endCarrier        bool
}

type callLeaveOperation struct {
	operationID           string
	callKey               string
	traceID               string
	clientID              string
	connectionID          string
	previousConnectionID  string
	matchedConnectionID   string
	channel               string
	endCarrier            bool
	completed             bool
	participantID         string
	logicalAnnounced      bool
	mediaAnnounced        bool
	logicalLeavePublished bool
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

func participantGeneration(participant *callParticipant, connectionID string) *callParticipant {
	for generation := participant; generation != nil; generation = generation.predecessor {
		if generation.connectionID == connectionID {
			return generation
		}
	}
	return nil
}

// joined decides admission under the registry lock and returns every channel
// whose exact Asterisk leg must be removed before that decision is complete.
// Events from an older connection generation are harmless because left() also
// matches connectionID.
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
			pendingDrops: make(map[string]callParticipant),
		}
		r.sessions[key] = session
	}
	if session.closingOperationID != "" {
		return callParticipantJoinResult{channelsToDrop: pendingDropChannels(session)}
	}
	logicalKey := logicalParticipantKey(clientID, participantID)
	if departure, found := session.departed[logicalKey]; found {
		// A delayed duplicate of the explicitly departed connection can never
		// rejoin. A genuinely new connection may join later, but only when its
		// engine timestamp is newer than the departure tombstone.
		_, explicitlyDeparted := departure.connectionIDs[connectionID]
		if explicitlyDeparted || !at.After(departure.at) {
			return callParticipantJoinResult{channelsToDrop: pendingDropChannels(session)}
		}
		if len(departure.abortedConnectionIDs) == 0 {
			delete(session.departed, logicalKey)
		} else {
			// A newer generation supersedes explicit-departure fencing, but an
			// aborted predecessor can still be reachable from the rollback chain.
			// Retain those tombstones until participant_left proves a generation
			// entered ConfBridge or the call ends.
			departure.connectionIDs = make(map[string]struct{}, len(departure.abortedConnectionIDs))
			for connectionID := range departure.abortedConnectionIDs {
				departure.connectionIDs[connectionID] = struct{}{}
			}
			session.departed[logicalKey] = departure
		}
	}
	previous, found := session.participants[logicalKey]
	if found && !at.After(previous.updatedAt) {
		if previous.connectionID != connectionID || previous.channel != channel {
			return callParticipantJoinResult{channelsToDrop: pendingDropChannels(session)}
		}
		return callParticipantJoinResult{channelsToDrop: pendingDropChannels(session), admitted: true}
	}
	if found {
		existingGeneration := participantGeneration(&previous, connectionID)
		if existingGeneration != nil && (existingGeneration != &previous || existingGeneration.admissionChannel != channel) {
			// A connection generation identifies exactly one Asterisk channel and
			// may only be idempotently replayed while it remains current. Reusing a
			// predecessor would resurrect a stale leg and make abort/leave ambiguous.
			return callParticipantJoinResult{channelsToDrop: pendingDropChannels(session)}
		}
	}
	if found && previous.connected && previous.connectionID != connectionID && previous.channel != "" {
		session.pendingDrops[previous.channel] = previous
	}
	replacement := callParticipant{
		clientID: clientID, participantID: participantID, connectionID: connectionID,
		channel: channel, admissionChannel: channel, connected: true, updatedAt: at,
	}
	if found {
		replacement.logicalAnnounced = previous.logicalAnnounced
	}
	if found && previous.connectionID != connectionID {
		// Keep the whole chain until a participant_left proves that a
		// generation entered ConfBridge. Several admission responses may be
		// lost before their durable aborts arrive, possibly out of order.
		predecessor := previous
		predecessor.connected = false
		predecessor.channel = ""
		replacement.predecessor = &predecessor
	}
	session.participants[logicalKey] = replacement
	return callParticipantJoinResult{channelsToDrop: pendingDropChannels(session), admitted: true}
}

func (r *callSessionRegistry) completeJoin(call Call, clientID, participantID, connectionID, channel string) (callParticipant, bool, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if clientID == "" {
		clientID = r.claims[participantClaimKey(call.TraceID, participantID)]
	}
	session := r.sessions[callSessionKey(call.Instance, call.EngineCallID)]
	if session == nil {
		return callParticipant{}, false, false
	}
	key := logicalParticipantKey(clientID, participantID)
	participant, found := session.participants[key]
	if !found || !participant.connected || participant.connectionID != connectionID || participant.admissionChannel != channel {
		return callParticipant{}, false, false
	}
	logicalJoined := !participant.logicalAnnounced
	mediaConnected := !participant.mediaAnnounced
	participant.logicalAnnounced = true
	participant.mediaAnnounced = true
	session.participants[key] = participant
	return participant, logicalJoined, mediaConnected
}

// abortJoin revokes a join whose synchronous admission response did not reach
// the waiting dialplan. The tombstone also fences a delayed copy of that exact
// request; a genuinely newer connection generation remains free to join.
func (r *callSessionRegistry) abortJoin(call Call, clientID, participantID, connectionID, channel string, at time.Time) bool {
	return r.abortJoinTransition(call, clientID, participantID, connectionID, channel, at).endCarrier
}

func (r *callSessionRegistry) abortJoinTransition(call Call, clientID, participantID, connectionID, channel string, at time.Time) callParticipantAbortResult {
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
			pendingDrops: make(map[string]callParticipant),
		}
		r.sessions[key] = session
	}
	pending, pendingFound := session.pendingDrops[channel]
	delete(session.pendingDrops, channel)
	logicalKey := logicalParticipantKey(clientID, participantID)
	departure := session.departed[logicalKey]
	removed := false
	participant, found := session.participants[logicalKey]
	result := callParticipantAbortResult{participant: participant}
	if pendingFound && pending.mediaAnnounced {
		result.participant = pending
		result.mediaDisconnected = true
	}
	if found {
		generation := participantGeneration(&participant, connectionID)
		if generation != nil && generation.admissionChannel != channel {
			// The manager rejected a second channel that tried to reuse an
			// already admitted connection generation. Even after that valid
			// generation becomes a predecessor, the rejected channel's abort must
			// not revoke it or fence the shared connection identifier.
			return result
		}
	}
	if found && participant.connectionID == connectionID {
		if !at.Before(participant.updatedAt) {
			// An abort for an older ambiguous admission may already have arrived
			// while a newer generation was current. Skip every such tombstoned
			// generation and restore the last still-valid logical owner.
			predecessor := participant.predecessor
			for predecessor != nil {
				if _, aborted := departure.abortedConnectionIDs[predecessor.connectionID]; !aborted {
					break
				}
				predecessor = predecessor.predecessor
			}
			if predecessor != nil {
				restored := *predecessor
				restored.connected = false
				restored.channel = ""
				session.participants[logicalKey] = restored
			} else {
				delete(session.participants, logicalKey)
			}
			removed = true
			result.logicalRemoved = predecessor == nil && participant.logicalAnnounced
			result.mediaDisconnected = participant.mediaAnnounced
		}
	}
	if departure.connectionIDs == nil {
		departure.connectionIDs = make(map[string]struct{})
	}
	if departure.abortedConnectionIDs == nil {
		departure.abortedConnectionIDs = make(map[string]struct{})
	}
	departure.connectionIDs[connectionID] = struct{}{}
	departure.abortedConnectionIDs[connectionID] = struct{}{}
	if departure.at.IsZero() || at.After(departure.at) {
		departure.at = at
	}
	session.departed[logicalKey] = departure
	abortOperationID := "abort:" + logicalKey + ":" + connectionID
	if session.closingOperationID == abortOperationID {
		result.endCarrier = true
		return result
	}
	if removed && len(session.participants) == 0 && session.explicitLeaveSeen && session.closingOperationID == "" {
		session.closingOperationID = abortOperationID
		result.endCarrier = true
	}
	return result
}

func pendingDropChannels(session *callSession) []string {
	channels := make([]string, 0, len(session.pendingDrops))
	for channel := range session.pendingDrops {
		channels = append(channels, channel)
	}
	sort.Strings(channels)
	return channels
}

func (r *callSessionRegistry) completeDrop(call Call, channel string) (callParticipant, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if session := r.sessions[callSessionKey(call.Instance, call.EngineCallID)]; session != nil {
		participant, found := session.pendingDrops[channel]
		delete(session.pendingDrops, channel)
		return participant, found && participant.mediaAnnounced
	}
	return callParticipant{}, false
}

func (r *callSessionRegistry) left(call Call, clientID, participantID, connectionID string, at time.Time) {
	r.leftChannel(call, clientID, participantID, connectionID, "", at)
}

func (r *callSessionRegistry) leftChannel(call Call, clientID, participantID, connectionID, channel string, at time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	session := r.sessions[callSessionKey(call.Instance, call.EngineCallID)]
	if session == nil {
		return false
	}
	pending, pendingFound := session.pendingDrops[channel]
	delete(session.pendingDrops, channel)
	logicalKey := logicalParticipantKey(clientID, participantID)
	participant, found := session.participants[logicalKey]
	if !found || participant.connectionID != connectionID || at.Before(participant.updatedAt) || !participant.connected {
		return pendingFound && pending.mediaAnnounced
	}
	mediaDisconnected := participant.mediaAnnounced
	participant.connected = false
	participant.mediaAnnounced = false
	participant.channel = ""
	participant.admissionChannel = ""
	// participant_left is only emitted for an admitted channel, so the
	// ambiguous-admission rollback history is no longer needed.
	participant.predecessor = nil
	participant.updatedAt = at
	session.participants[logicalKey] = participant
	delete(session.departed, logicalKey)
	return mediaDisconnected
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
		stableKey := logicalParticipantKey(clientID, participantID)
		if provisionalDeparture, found := session.departed[provisionalKey]; found {
			delete(session.departed, provisionalKey)
			stableDeparture := session.departed[stableKey]
			if stableDeparture.connectionIDs == nil {
				stableDeparture.connectionIDs = make(map[string]struct{})
			}
			if stableDeparture.abortedConnectionIDs == nil {
				stableDeparture.abortedConnectionIDs = make(map[string]struct{})
			}
			for connectionID := range provisionalDeparture.connectionIDs {
				stableDeparture.connectionIDs[connectionID] = struct{}{}
			}
			for connectionID := range provisionalDeparture.abortedConnectionIDs {
				stableDeparture.abortedConnectionIDs[connectionID] = struct{}{}
			}
			if stableDeparture.at.IsZero() || provisionalDeparture.at.After(stableDeparture.at) {
				stableDeparture.at = provisionalDeparture.at
			}
			session.departed[stableKey] = stableDeparture
		}
		participant, found := session.participants[provisionalKey]
		if !found {
			return true
		}
		delete(session.participants, provisionalKey)
		participant.clientID = clientID
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
	return r.beginLeaveWithPrevious(call, clientID, connectionID, "", operationID)
}

func (r *callSessionRegistry) beginLeaveWithPrevious(call Call, clientID, connectionID, previousConnectionID, operationID string) (callLeaveOperation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := callSessionKey(call.Instance, call.EngineCallID)
	if existing := r.operations[operationID]; existing != nil {
		if existing.callKey != key || existing.clientID != clientID || existing.connectionID != connectionID || existing.previousConnectionID != previousConnectionID {
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
	matchedConnectionID := ""
	if found {
		if participant.connectionID != connectionID && participant.connectionID != previousConnectionID {
			return callLeaveOperation{}, errCallParticipantNotFound
		}
		matchedConnectionID = participant.connectionID
	} else {
		departure := session.departed[logicalKey]
		if _, aborted := departure.abortedConnectionIDs[connectionID]; aborted {
			matchedConnectionID = connectionID
		} else if _, aborted := departure.abortedConnectionIDs[previousConnectionID]; aborted {
			matchedConnectionID = previousConnectionID
		} else {
			return callLeaveOperation{}, errCallParticipantNotFound
		}
		participant = callParticipant{clientID: clientID, connectionID: matchedConnectionID}
	}
	delete(session.participants, logicalKey)
	session.explicitLeaveSeen = true
	connectionIDs := map[string]struct{}{participant.connectionID: {}}
	if connectionID != "" {
		connectionIDs[connectionID] = struct{}{}
	}
	if previousConnectionID != "" {
		connectionIDs[previousConnectionID] = struct{}{}
	}
	session.departed[logicalKey] = callParticipantDeparture{connectionIDs: connectionIDs, at: time.Now().UTC()}
	operation := &callLeaveOperation{
		operationID: operationID, callKey: key, traceID: call.TraceID,
		clientID: clientID, connectionID: connectionID, previousConnectionID: previousConnectionID,
		matchedConnectionID: matchedConnectionID, channel: participant.channel,
		endCarrier:       len(session.participants) == 0,
		participantID:    participant.participantID,
		logicalAnnounced: participant.logicalAnnounced, mediaAnnounced: participant.mediaAnnounced,
	}
	if operation.endCarrier {
		session.closingOperationID = operationID
	}
	r.operations[operationID] = operation
	return *operation, nil
}

func (r *callSessionRegistry) markLogicalLeavePublished(operationID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	operation := r.operations[operationID]
	if operation == nil || operation.logicalLeavePublished {
		return false
	}
	operation.logicalLeavePublished = true
	return true
}

func (r *callSessionRegistry) completeLeave(operationID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if operation := r.operations[operationID]; operation != nil {
		if operation.completed {
			return false
		}
		operation.completed = true
		return true
	}
	return false
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
