package app

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"
)

const smsDeliveryReportWindow = 24 * time.Hour
const smsServiceCentreTimeTolerance = 15 * time.Minute
const smsRPReferenceCorrelationWindow = 15 * time.Minute

var (
	ErrSMSReportNotCorrelated = errors.New("SMS delivery report could not be correlated")
	ErrSMSReportAmbiguous     = errors.New("SMS delivery report matched more than one message")
)

type SMSProtocolEvent struct {
	Instance          string
	Phase             string
	MessageID         string
	Destination       string
	Part              int
	Parts             int
	RPReference       *int
	TPReference       *int
	Status            *int
	Cause             *int
	ServiceCentreTime *time.Time
}

func cloneMessage(message Message) Message {
	message.Parts = append([]SMSPartStatus(nil), message.Parts...)
	for index := range message.Parts {
		if message.Parts[index].RPReference != nil {
			value := *message.Parts[index].RPReference
			message.Parts[index].RPReference = &value
		}
		if message.Parts[index].TPReference != nil {
			value := *message.Parts[index].TPReference
			message.Parts[index].TPReference = &value
		}
		if message.Parts[index].SubmittedAt != nil {
			value := *message.Parts[index].SubmittedAt
			message.Parts[index].SubmittedAt = &value
		}
		if message.Parts[index].ServiceCentreTime != nil {
			value := *message.Parts[index].ServiceCentreTime
			message.Parts[index].ServiceCentreTime = &value
		}
	}
	if message.DeliveryReportDeadline != nil {
		value := *message.DeliveryReportDeadline
		message.DeliveryReportDeadline = &value
	}
	return message
}

// UpdateMessage serializes protocol callbacks with the original submission and
// rolls memory back when the durable state file cannot be replaced.
func (s *Store) UpdateMessage(id string, update func(*Message) bool) (Message, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, ok := s.messages[id]
	if !ok {
		return Message{}, false, nil
	}
	previous := cloneMessage(current)
	next := cloneMessage(current)
	if !update(&next) {
		return cloneMessage(current), false, nil
	}
	s.messages[id] = next
	if err := s.flushLocked(); err != nil {
		s.messages[id] = previous
		return Message{}, false, err
	}
	return cloneMessage(next), true, nil
}

func (s *Store) ApplySMSProtocolEvent(event SMSProtocolEvent, now time.Time) (Message, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now = now.UTC()
	id, err := s.correlateSMSLocked(event, now)
	if err != nil {
		return Message{}, false, err
	}
	previous := cloneMessage(s.messages[id])
	next := cloneMessage(previous)
	changed, err := applySMSProtocolEvent(&next, event, now.UTC())
	if err != nil || !changed {
		return cloneMessage(previous), false, err
	}
	s.messages[id] = next
	if err := s.flushLocked(); err != nil {
		s.messages[id] = previous
		return Message{}, false, err
	}
	return cloneMessage(next), true, nil
}

func (s *Store) correlateSMSLocked(event SMSProtocolEvent, eventTime time.Time) (string, error) {
	id := strings.TrimSpace(event.MessageID)
	phase := strings.ToLower(strings.TrimSpace(event.Phase))
	isRPResponse := phase == "rp_ack" || phase == "rp_error"
	if id != "" && !isRPResponse {
		message, ok := s.messages[id]
		if !ok || message.Direction != DirectionOutgoing {
			return "", ErrSMSReportNotCorrelated
		}
		if event.Destination != "" && strings.TrimSpace(message.To) != strings.TrimSpace(event.Destination) {
			return "", ErrSMSReportNotCorrelated
		}
		// These events describe the local operation that created the references,
		// so the manager-generated message ID is authoritative. Responses are
		// validated against all persisted reference owners below because
		// Asterisk's in-memory table is empty after a restart.
		if phase == "submitted" || phase == "local_failure" {
			return id, nil
		}
	}
	candidates := s.smsCorrelationCandidatesLocked(event, eventTime)
	if len(candidates) > 1 {
		return "", ErrSMSReportAmbiguous
	}
	for candidate := range candidates {
		// A volatile Asterisk track may attach a stale response to a newly
		// reused reference. Persisted timestamp/reference evidence wins.
		return candidate, nil
	}
	return "", ErrSMSReportNotCorrelated
}

func (s *Store) smsCorrelationCandidatesLocked(event SMSProtocolEvent, eventTime time.Time) map[string]struct{} {
	phase := strings.ToLower(strings.TrimSpace(event.Phase))
	candidates := make(map[string]struct{})
	for _, message := range s.messages {
		if message.Direction != DirectionOutgoing {
			continue
		}
		// RP-ACK and RP-ERROR carry no destination on the wire. Asterisk can
		// only infer it from volatile track state, so it must never constrain
		// persistent ownership after a restart.
		if phase != "rp_ack" && phase != "rp_error" && event.Destination != "" &&
			strings.TrimSpace(message.To) != strings.TrimSpace(event.Destination) {
			continue
		}
		for _, part := range message.Parts {
			if strings.TrimSpace(event.Instance) != strings.TrimSpace(part.Instance) {
				continue
			}
			if part.SubmittedAt == nil {
				continue
			}
			switch phase {
			case "rp_ack", "rp_error":
				if !sameReference(part.RPReference, event.RPReference) ||
					!smsTimesWithin(*part.SubmittedAt, eventTime, smsRPReferenceCorrelationWindow) {
					continue
				}
			case "status_report":
				if !sameReference(part.TPReference, event.TPReference) {
					continue
				}
				if event.ServiceCentreTime != nil {
					if !smsTimesClose(*part.SubmittedAt, *event.ServiceCentreTime) {
						continue
					}
				} else if !smsTimesWithin(*part.SubmittedAt, eventTime, smsDeliveryReportWindow) {
					continue
				}
			default:
				continue
			}
			candidates[message.ID] = struct{}{}
			break
		}
	}
	return candidates
}

func sameReference(left, right *int) bool {
	return left != nil && right != nil && *left == *right
}

func smsTimesWithin(left, right time.Time, window time.Duration) bool {
	delta := left.Sub(right)
	if delta < 0 {
		delta = -delta
	}
	return delta <= window
}

func smsTimesClose(left, right time.Time) bool {
	return smsTimesWithin(left, right, smsServiceCentreTimeTolerance)
}

func applySMSProtocolEvent(message *Message, event SMSProtocolEvent, now time.Time) (bool, error) {
	if message.Direction != DirectionOutgoing {
		return false, errors.New("SMS protocol event targets a non-outgoing message")
	}
	phase := strings.ToLower(strings.TrimSpace(event.Phase))
	if phase != "submitted" && phase != "local_failure" && phase != "rp_ack" && phase != "rp_error" && phase != "status_report" {
		return false, fmt.Errorf("unsupported SMS protocol phase %q", event.Phase)
	}
	if event.Part <= 0 {
		event.Part = 1
	}
	if event.Parts < event.Part {
		event.Parts = event.Part
	}
	if event.Parts <= 0 {
		event.Parts = 1
	}
	index := smsPartIndex(message.Parts, event)
	changed := false
	if index < 0 {
		message.Parts = append(message.Parts, SMSPartStatus{Part: event.Part, Parts: event.Parts})
		index = len(message.Parts) - 1
		changed = true
	}
	part := &message.Parts[index]
	if (phase == "submitted" || phase == "local_failure") &&
		(referenceConflicts(part.RPReference, event.RPReference) || referenceConflicts(part.TPReference, event.TPReference)) {
		return false, errors.New("SMS submission references conflict with persisted part mapping")
	}
	changed = setString(&part.Instance, strings.TrimSpace(event.Instance)) || changed
	changed = setString(&part.Recipient, strings.TrimSpace(event.Destination)) || changed
	if part.Parts < event.Parts {
		part.Parts = event.Parts
		changed = true
	}
	changed = setReference(&part.RPReference, event.RPReference) || changed
	changed = setReference(&part.TPReference, event.TPReference) || changed
	if phase == "submitted" {
		changed = setTime(&part.SubmittedAt, &now) || changed
	}
	if phase == "status_report" {
		changed = setTime(&part.ServiceCentreTime, event.ServiceCentreTime) || changed
	}

	state, statusCode, failureCode, failureReason, detail, err := smsProtocolOutcome(event)
	if err != nil {
		return false, err
	}
	if shouldReplaceSMSPartState(part.State, state, phase) {
		if part.State != state {
			part.State = state
			changed = true
		}
		changed = setStringValue(&part.StatusCode, statusCode) || changed
		changed = setStringValue(&part.FailureCode, failureCode) || changed
		changed = setStringValue(&part.FailureReason, failureReason) || changed
		changed = setStringValue(&part.Detail, detail) || changed
	}
	if !changed {
		return false, nil
	}
	part.UpdatedAt = now
	message.DeliveryReportRequested = true
	sort.Slice(message.Parts, func(i, j int) bool { return message.Parts[i].Part < message.Parts[j].Part })
	recomputeSMSMessage(message)
	if message.DeliveryState == "pending" {
		deadline := now.Add(smsDeliveryReportWindow)
		message.DeliveryReportDeadline = &deadline
	} else {
		message.DeliveryReportDeadline = nil
	}
	message.UpdatedAt = now
	return true, nil
}

func referenceConflicts(current, next *int) bool {
	return current != nil && next != nil && *current != *next
}

func smsPartIndex(parts []SMSPartStatus, event SMSProtocolEvent) int {
	phase := strings.ToLower(strings.TrimSpace(event.Phase))
	for index, part := range parts {
		matched := false
		switch phase {
		case "rp_ack", "rp_error":
			matched = sameReference(part.RPReference, event.RPReference)
		case "status_report":
			matched = sameReference(part.TPReference, event.TPReference)
		default:
			matched = sameReference(part.RPReference, event.RPReference) || sameReference(part.TPReference, event.TPReference)
		}
		if matched {
			return index
		}
	}
	for index, part := range parts {
		if event.Part > 0 && part.Part == event.Part {
			return index
		}
	}
	return -1
}

func setString(target *string, value string) bool {
	if value == "" || *target == value {
		return false
	}
	if *target != "" {
		return false
	}
	*target = value
	return true
}

func setStringValue(target *string, value string) bool {
	if *target == value {
		return false
	}
	*target = value
	return true
}

func setReference(target **int, value *int) bool {
	if value == nil || (*target != nil && **target == *value) {
		return false
	}
	if *target != nil {
		return false
	}
	copy := *value
	*target = &copy
	return true
}

func setTime(target **time.Time, value *time.Time) bool {
	if value == nil || *target != nil {
		return false
	}
	copy := value.UTC()
	*target = &copy
	return true
}

func shouldReplaceSMSPartState(current, next, phase string) bool {
	if current == "delivered" || current == "failed" {
		return current == next
	}
	if current == "unknown" && phase != "status_report" {
		return next == current
	}
	rank := map[string]int{"": 0, "submitted": 1, "accepted": 2, "pending": 2, "unknown": 3, "delivered": 4, "failed": 4}
	return rank[next] >= rank[current]
}

func smsProtocolOutcome(event SMSProtocolEvent) (state, statusCode, failureCode, failureReason, detail string, err error) {
	switch strings.ToLower(strings.TrimSpace(event.Phase)) {
	case "submitted":
		return "submitted", "", "", "", "Submitted to the IMS transport; awaiting carrier acknowledgement.", nil
	case "local_failure":
		return "failed", "", "LOCAL_SEND_FAILED", "Asterisk could not transmit the RP-DATA request", "The local IMS transport failed before carrier acknowledgement.", nil
	case "rp_ack":
		return "accepted", "", "", "", "The carrier message service acknowledged the RP-DATA transfer.", nil
	case "rp_error":
		if event.Cause == nil {
			return "failed", "", "RP_ERROR", "carrier returned RP-ERROR without a decodable cause", "The carrier rejected the RP-DATA transfer without a decodable cause.", nil
		}
		code := formatSMSCode(*event.Cause)
		reason := rpCauseReason(*event.Cause)
		return "failed", "", code, reason, "The carrier rejected the RP-DATA transfer: " + reason, nil
	case "status_report":
		if event.Status == nil {
			return "", "", "", "", "", errors.New("SMS status report is missing TP-ST")
		}
		return tpStatusOutcome(*event.Status)
	default:
		return "", "", "", "", "", fmt.Errorf("unsupported SMS protocol phase %q", event.Phase)
	}
}

func tpStatusOutcome(status int) (state, statusCode, failureCode, failureReason, detail string, err error) {
	if status < 0 || status > 255 {
		return "", "", "", "", "", fmt.Errorf("invalid TP-ST value %d", status)
	}
	code := formatSMSCode(status)
	switch {
	case status == 0x00:
		return "delivered", code, "", "", "The receiving device acknowledged the message. This is not a read receipt.", nil
	case status == 0x01:
		return "unknown", code, "", "", "The service centre forwarded the message but could not confirm delivery.", nil
	case status == 0x02:
		return "unknown", code, "", "", "The service centre replaced the message; destination delivery is unconfirmed.", nil
	case status >= 0x20 && status <= 0x3f:
		reason := tpStatusReason(status)
		return "pending", code, "", "", "Temporary delivery problem; the service centre is still retrying: " + reason, nil
	case status >= 0x40 && status <= 0x7f:
		reason := tpStatusReason(status)
		return "failed", code, code, reason, "The service centre stopped delivery attempts: " + reason, nil
	default:
		return "unknown", code, "", "", "The carrier returned an unstandardized or reserved terminal delivery status (" + code + ").", nil
	}
}

func recomputeSMSMessage(message *Message) {
	if len(message.Parts) == 0 {
		return
	}
	expected := 1
	failed, delivered, unknown, accepted, submitted := 0, 0, 0, 0, 0
	for _, part := range message.Parts {
		if part.Parts > expected {
			expected = part.Parts
		}
		switch part.State {
		case "failed":
			failed++
		case "delivered":
			delivered++
		case "unknown":
			unknown++
		case "accepted", "pending":
			accepted++
		case "submitted":
			submitted++
		}
	}
	message.FailureCode, message.FailureReason = "", ""
	if failed > 0 {
		message.State, message.DeliveryState = "failed", "failed"
		for _, part := range message.Parts {
			if part.State == "failed" {
				message.FailureCode, message.FailureReason, message.StatusDetail = part.FailureCode, part.FailureReason, part.Detail
				break
			}
		}
		return
	}
	if len(message.Parts) >= expected && delivered == expected {
		message.State, message.DeliveryState = "delivered", "delivered"
		message.StatusDetail = "Every message segment was acknowledged by the receiving device. This is not a read receipt."
		return
	}
	if len(message.Parts) >= expected && delivered+unknown == expected {
		message.State, message.DeliveryState = "accepted", "unknown"
		message.StatusDetail = "The carrier finished processing the message without confirming destination delivery."
		return
	}
	message.DeliveryState = "pending"
	allSegmentsAccepted := len(message.Parts) >= expected && delivered+unknown+accepted == expected
	if allSegmentsAccepted {
		message.State = "accepted"
	} else if submitted+delivered+unknown+accepted > 0 {
		message.State = "submitted"
	}
	if !allSegmentsAccepted && (len(message.Parts) < expected || delivered+unknown+accepted < expected) {
		message.StatusDetail = fmt.Sprintf("Submitted %d of %d message segments; waiting for carrier acknowledgement of every segment.", len(message.Parts), expected)
	} else {
		message.StatusDetail = latestSMSPartDetail(message.Parts)
	}
}

func latestSMSPartDetail(parts []SMSPartStatus) string {
	var latest SMSPartStatus
	for _, part := range parts {
		if part.Detail != "" && (latest.UpdatedAt.IsZero() || part.UpdatedAt.After(latest.UpdatedAt)) {
			latest = part
		}
	}
	return latest.Detail
}

func applySMSSubmitResult(message *Message, result SMSSubmitResult, submitErr error, now time.Time) bool {
	if message.Direction != DirectionOutgoing {
		return false
	}
	before := cloneMessage(*message)
	message.DeliveryReportRequested = message.DeliveryReportRequested || result.DeliveryReportExpected
	if result.DeliveryReportExpected && message.DeliveryReportDeadline == nil {
		deadline := now.Add(smsDeliveryReportWindow)
		message.DeliveryReportDeadline = &deadline
	}
	terminal := message.State == "delivered" || message.State == "failed"
	if submitErr != nil && !errors.Is(submitErr, ErrDeliveryUnconfirmed) {
		if !terminal {
			message.State, message.DeliveryState = "failed", "failed"
			message.FailureCode = result.FailureCode
			message.FailureReason = firstNonEmpty(result.FailureReason, submitErr.Error())
			message.StatusDetail = firstNonEmpty(result.Detail, "The local SMS submission failed.")
			message.DeliveryReportDeadline = nil
		}
	} else if !terminal {
		switch firstNonEmpty(result.State, "submitted") {
		case "delivered":
			// Only an adapter's explicit delivered-and-verified result reaches
			// this state. Carrier acceptance alone never takes this path.
			message.State = "delivered"
			message.DeliveryState = "delivered"
			message.DeliveryReportDeadline = nil
			message.FailureCode, message.FailureReason = "", ""
			message.StatusDetail = result.Detail
		case "failed":
			message.State, message.DeliveryState = "failed", "failed"
			message.DeliveryReportDeadline = nil
			message.FailureCode = result.FailureCode
			message.FailureReason = firstNonEmpty(result.FailureReason, "The SMS adapter rejected the message.")
			message.StatusDetail = firstNonEmpty(result.Detail, message.FailureReason)
		case "accepted":
			if message.State != "accepted" || len(message.Parts) == 0 {
				message.State = "accepted"
			}
			if !result.DeliveryReportExpected && len(message.Parts) == 0 {
				message.DeliveryState = "unknown"
				message.DeliveryReportDeadline = nil
			}
			if len(message.Parts) == 0 {
				message.StatusDetail = result.Detail
			}
		default:
			// A late local submission result must not regress an RP-ACK or a
			// TP status report that arrived first.
			if message.State != "accepted" && len(message.Parts) == 0 {
				message.State = "submitted"
				if result.DeliveryReportExpected {
					message.DeliveryState = "pending"
				} else {
					message.DeliveryState = "unknown"
					message.DeliveryReportDeadline = nil
				}
				message.StatusDetail = result.Detail
			}
		}
	}
	if message.State == "accepted" && !result.DeliveryReportExpected && message.DeliveryState == "pending" && len(message.Parts) == 0 {
		message.DeliveryState = "unknown"
		message.DeliveryReportDeadline = nil
	}
	if !messagesEqualForSMS(before, *message) {
		message.UpdatedAt = now
		return true
	}
	return false
}

func messagesEqualForSMS(left, right Message) bool {
	left.UpdatedAt, right.UpdatedAt = time.Time{}, time.Time{}
	return reflect.DeepEqual(left, right)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func (s *Store) ExpireSMSDeliveryReports(now time.Time) ([]Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now = now.UTC()
	previous := make(map[string]Message)
	updated := make([]Message, 0)
	for id, stored := range s.messages {
		if stored.Direction != DirectionOutgoing || stored.DeliveryState != "pending" || stored.DeliveryReportDeadline == nil || stored.DeliveryReportDeadline.After(now) {
			continue
		}
		previous[id] = cloneMessage(stored)
		message := cloneMessage(stored)
		for index := range message.Parts {
			if message.Parts[index].State != "delivered" && message.Parts[index].State != "failed" {
				message.Parts[index].State = "unknown"
				message.Parts[index].Detail = "No delivery report was received before the reporting window expired."
				message.Parts[index].UpdatedAt = now
			}
		}
		message.DeliveryState = "unknown"
		if message.State == "sending" {
			message.State = "submitted"
		}
		message.StatusDetail = "No delivery report was received; the carrier may not support or may have delayed it."
		message.UpdatedAt = now
		s.messages[id] = message
		updated = append(updated, cloneMessage(message))
	}
	if len(updated) == 0 {
		return nil, nil
	}
	if err := s.flushLocked(); err != nil {
		for id, message := range previous {
			s.messages[id] = message
		}
		return nil, err
	}
	return updated, nil
}

func normalizeStoredMessage(message Message, now time.Time) Message {
	if message.Direction != DirectionOutgoing {
		return message
	}
	if message.DeliveryState == "" {
		switch message.State {
		case "sending":
			message.DeliveryState = "pending"
		case "submitted", "accepted":
			message.DeliveryState = "unknown"
			message.StatusDetail = "This legacy message has no persisted destination delivery report."
		case "delivered":
			message.DeliveryState = "delivered"
			message.StatusDetail = "The legacy adapter recorded verified delivery; this is not a read receipt."
		case "failed":
			message.DeliveryState = "failed"
		}
	}
	if message.State == "sending" {
		message.State = "unknown"
		message.DeliveryState = "unknown"
		message.DeliveryReportDeadline = nil
		message.StatusDetail = "The manager restarted before the submission outcome was persisted; carrier submission is unknown."
		return message
	}
	if message.DeliveryState == "pending" && message.DeliveryReportDeadline == nil {
		basis := message.UpdatedAt
		if basis.IsZero() {
			basis = message.CreatedAt
		}
		deadline := basis.Add(smsDeliveryReportWindow)
		if basis.IsZero() || !deadline.After(now) {
			message.DeliveryState = "unknown"
			message.State = "unknown"
			message.StatusDetail = "The manager restarted before the submission outcome was persisted; carrier submission is unknown."
		} else {
			message.DeliveryReportDeadline = &deadline
		}
	}
	return message
}

func formatSMSCode(value int) string { return fmt.Sprintf("0x%02X", value&0xff) }

func rpCauseReason(cause int) string {
	reasons := map[int]string{
		1: "unassigned number", 8: "operator-determined barring", 10: "call barred",
		21: "short-message transfer rejected", 22: "memory capacity exceeded",
		27: "destination out of order", 28: "unidentified subscriber", 29: "facility rejected",
		30: "unknown subscriber", 38: "network out of order", 41: "temporary failure",
		42: "network congestion", 47: "resources unavailable", 50: "facility not subscribed",
		69: "facility not implemented", 81: "invalid message reference",
		95: "invalid message", 96: "mandatory information missing", 97: "unknown message type",
		98: "message incompatible with protocol state", 99: "unknown information element",
		111: "protocol error", 127: "interworking error",
	}
	if reason := reasons[cause]; reason != "" {
		return reason
	}
	return "RP error cause " + strconv.Itoa(cause)
}

func tpStatusReason(status int) string {
	reasons := map[int]string{
		0x20: "network congestion", 0x21: "destination device busy", 0x22: "destination device did not respond",
		0x23: "service rejected", 0x24: "quality of service unavailable", 0x25: "destination device error",
		0x40: "remote procedure error", 0x41: "incompatible destination", 0x42: "connection rejected",
		0x43: "destination not obtainable", 0x44: "no internetworking available", 0x45: "validity period expired",
		0x46: "deleted by originator", 0x47: "deleted by service-centre administration", 0x48: "message does not exist",
		0x60: "network congestion", 0x61: "destination device busy", 0x62: "destination device did not respond",
		0x63: "service rejected", 0x64: "quality of service unavailable", 0x65: "destination device error",
	}
	if reason := reasons[status]; reason != "" {
		return reason
	}
	return "delivery status " + formatSMSCode(status)
}
