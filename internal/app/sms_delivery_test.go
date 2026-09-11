package app

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func smsInt(value int) *int { return &value }

func newOutgoingSMS(t *testing.T) (*Store, Message, time.Time) {
	t.Helper()
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	store, err := OpenStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	message := Message{
		ID: "sms-one", Direction: DirectionOutgoing, To: "+34600000000", Text: "hello",
		State: "sending", DeliveryState: "pending", CreatedAt: now, UpdatedAt: now,
	}
	if err := store.SaveMessage(message); err != nil {
		t.Fatal(err)
	}
	return store, message, now
}

func submitSMSPart(t *testing.T, store *Store, messageID, instance, destination string, part, parts, rp, tp int, when time.Time) {
	t.Helper()
	_, _, err := store.ApplySMSProtocolEvent(SMSProtocolEvent{
		Phase: "submitted", MessageID: messageID, Instance: instance, Destination: destination,
		Part: part, Parts: parts, RPReference: smsInt(rp), TPReference: smsInt(tp),
	}, when)
	if err != nil {
		t.Fatalf("submit SMS part %d: %v", part, err)
	}
}

func TestSMSProtocolStatesAreMonotonicAndIdempotent(t *testing.T) {
	store, _, now := newOutgoingSMS(t)
	submitSMSPart(t, store, "sms-one", "1", "+34600000000", 1, 1, 0, 0, now)
	event := SMSProtocolEvent{
		Phase: "rp_ack", MessageID: "sms-one", Instance: "1", Destination: "+34600000000",
		Part: 1, Parts: 1, RPReference: smsInt(0), TPReference: smsInt(0),
	}
	message, changed, err := store.ApplySMSProtocolEvent(event, now.Add(time.Second))
	if err != nil || !changed || message.State != "accepted" || message.DeliveryState != "pending" {
		t.Fatalf("ack result=%+v changed=%t err=%v", message, changed, err)
	}
	if _, changed, err := store.ApplySMSProtocolEvent(event, now.Add(2*time.Second)); err != nil || changed {
		t.Fatalf("duplicate changed=%t err=%v", changed, err)
	}
	message, changed, err = store.UpdateMessage("sms-one", func(current *Message) bool {
		return applySMSSubmitResult(current, SMSSubmitResult{State: "submitted", DeliveryReportExpected: true}, nil, now.Add(3*time.Second))
	})
	if err != nil || changed || message.State != "accepted" {
		t.Fatalf("late local submission regressed state: %+v changed=%t err=%v", message, changed, err)
	}
}

func TestSMSStatusReportIsRetriedAfterSubmittedAndCompletesMultipart(t *testing.T) {
	store, _, now := newOutgoingSMS(t)
	first := SMSProtocolEvent{Phase: "status_report", MessageID: "sms-one", Instance: "1", Destination: "+34600000000", Part: 1, Parts: 2, TPReference: smsInt(10), Status: smsInt(0)}
	_, changed, err := store.ApplySMSProtocolEvent(first, now.Add(time.Second))
	if !errors.Is(err, ErrSMSReportNotCorrelated) || changed {
		t.Fatalf("premature report changed=%t err=%v", changed, err)
	}
	lateSubmitted := SMSProtocolEvent{Phase: "submitted", MessageID: "sms-one", Instance: "1", Destination: "+34600000000", Part: 1, Parts: 2, RPReference: smsInt(9), TPReference: smsInt(10)}
	message, _, err := store.ApplySMSProtocolEvent(lateSubmitted, now.Add(2*time.Second))
	if err != nil || message.Parts[0].State != "submitted" {
		t.Fatalf("submitted mapping missing: %+v err=%v", message, err)
	}
	if message, _, err = store.ApplySMSProtocolEvent(first, now.Add(time.Second)); err != nil || message.Parts[0].State != "delivered" {
		t.Fatalf("retried report did not win: %+v err=%v", message, err)
	}
	submitSMSPart(t, store, "sms-one", "1", "+34600000000", 2, 2, 10, 11, now.Add(3*time.Second))
	second := SMSProtocolEvent{Phase: "status_report", MessageID: "sms-one", Instance: "1", Destination: "+34600000000", Part: 2, Parts: 2, TPReference: smsInt(11), Status: smsInt(0)}
	message, _, err = store.ApplySMSProtocolEvent(second, now.Add(4*time.Second))
	if err != nil || message.State != "delivered" || message.DeliveryState != "delivered" || len(message.Parts) != 2 {
		t.Fatalf("multipart result=%+v err=%v", message, err)
	}
}

func TestSMSStatusReportMapsRetryFailureAndUnconfirmed(t *testing.T) {
	tests := []struct {
		status       int
		wantState    string
		wantDelivery string
		wantFailure  string
	}{
		{status: 0x20, wantState: "accepted", wantDelivery: "pending"},
		{status: 0x01, wantState: "accepted", wantDelivery: "unknown"},
		{status: 0x45, wantState: "failed", wantDelivery: "failed", wantFailure: "0x45"},
		{status: 0x80, wantState: "accepted", wantDelivery: "unknown"},
	}
	for _, test := range tests {
		t.Run(formatSMSCode(test.status), func(t *testing.T) {
			store, _, now := newOutgoingSMS(t)
			submitSMSPart(t, store, "sms-one", "", "+34600000000", 1, 1, 6, 7, now)
			message, _, err := store.ApplySMSProtocolEvent(SMSProtocolEvent{
				Phase: "status_report", MessageID: "sms-one", Destination: "+34600000000",
				Part: 1, Parts: 1, TPReference: smsInt(7), Status: smsInt(test.status),
			}, now.Add(time.Second))
			if err != nil || message.State != test.wantState || message.DeliveryState != test.wantDelivery || message.FailureCode != test.wantFailure {
				t.Fatalf("message=%+v err=%v", message, err)
			}
		})
	}
}

func TestSMSRPErrorPersistsExactCauseAndExplanation(t *testing.T) {
	store, _, now := newOutgoingSMS(t)
	submitSMSPart(t, store, "sms-one", "", "+34600000000", 1, 1, 4, 5, now)
	message, _, err := store.ApplySMSProtocolEvent(SMSProtocolEvent{
		Phase: "rp_error", MessageID: "sms-one", Destination: "+34600000000",
		Part: 1, Parts: 1, RPReference: smsInt(4), TPReference: smsInt(5), Cause: smsInt(42),
	}, now.Add(time.Second))
	if err != nil || message.State != "failed" || message.FailureCode != "0x2A" || message.FailureReason != "network congestion" {
		t.Fatalf("message=%+v err=%v", message, err)
	}
}

func TestSMSReportCorrelatesByPersistedReferencesAfterRestart(t *testing.T) {
	store, _, now := newOutgoingSMS(t)
	_, _, err := store.ApplySMSProtocolEvent(SMSProtocolEvent{
		Phase: "submitted", MessageID: "sms-one", Instance: "engine-1", Destination: "+34600000000",
		Part: 1, Parts: 1, RPReference: smsInt(17), TPReference: smsInt(23),
	}, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenStore(store.path)
	if err != nil {
		t.Fatal(err)
	}
	message, changed, err := reopened.ApplySMSProtocolEvent(SMSProtocolEvent{
		Phase: "status_report", Instance: "engine-1", Destination: "+34600000000",
		Part: 1, Parts: 1, TPReference: smsInt(23), Status: smsInt(0),
	}, now.Add(2*time.Second))
	if err != nil || !changed || message.ID != "sms-one" || message.State != "delivered" {
		t.Fatalf("message=%+v changed=%t err=%v", message, changed, err)
	}
}

func TestSMSMultipartReportUsesReferenceBeforeDefaultPartAfterRestart(t *testing.T) {
	store, _, now := newOutgoingSMS(t)
	for part, reference := range []int{23, 24} {
		_, _, err := store.ApplySMSProtocolEvent(SMSProtocolEvent{
			Phase: "submitted", MessageID: "sms-one", Instance: "engine-1", Destination: "+34600000000",
			Part: part + 1, Parts: 2, RPReference: smsInt(17 + part), TPReference: smsInt(reference),
		}, now.Add(time.Duration(part+1)*time.Second))
		if err != nil {
			t.Fatal(err)
		}
	}
	reopened, err := OpenStore(store.path)
	if err != nil {
		t.Fatal(err)
	}
	message, changed, err := reopened.ApplySMSProtocolEvent(SMSProtocolEvent{
		Phase: "status_report", Instance: "engine-1", Destination: "+34600000000",
		// After an Asterisk restart the volatile part index is unavailable and
		// the engine reports 1; the persisted TP reference still identifies part 2.
		Part: 1, Parts: 2, TPReference: smsInt(24), Status: smsInt(0),
	}, now.Add(3*time.Second))
	if err != nil || !changed || len(message.Parts) != 2 || message.Parts[1].Part != 2 || message.Parts[1].State != "delivered" {
		t.Fatalf("message=%+v changed=%t err=%v", message, changed, err)
	}
}

func TestSMSStatusMessageIDWithWrongDestinationIsRejected(t *testing.T) {
	store, _, now := newOutgoingSMS(t)
	submitSMSPart(t, store, "sms-one", "engine-1", "+34600000000", 1, 1, 4, 5, now)
	_, changed, err := store.ApplySMSProtocolEvent(SMSProtocolEvent{
		Phase: "status_report", MessageID: "sms-one", Instance: "engine-1", Destination: "+34999999999",
		Part: 1, Parts: 1, TPReference: smsInt(5), Status: smsInt(0),
	}, now.Add(time.Second))
	if !errors.Is(err, ErrSMSReportNotCorrelated) || changed {
		t.Fatalf("changed=%t err=%v", changed, err)
	}
}

func TestSMSUnknownExplicitMessageIDNeverFallsBackToAnotherMessage(t *testing.T) {
	store, _, now := newOutgoingSMS(t)
	_, _, err := store.ApplySMSProtocolEvent(SMSProtocolEvent{
		Phase: "submitted", MessageID: "sms-one", Instance: "engine-1", Destination: "+34600000000",
		Part: 1, Parts: 1, RPReference: smsInt(3), TPReference: smsInt(7),
	}, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	_, changed, err := store.ApplySMSProtocolEvent(SMSProtocolEvent{
		Phase: "status_report", MessageID: "does-not-exist", Instance: "engine-1", Destination: "+34600000000",
		Part: 1, Parts: 1, TPReference: smsInt(7), Status: smsInt(0),
	}, now.Add(2*time.Second))
	if !errors.Is(err, ErrSMSReportNotCorrelated) || changed {
		t.Fatalf("changed=%t err=%v", changed, err)
	}
}

func TestSMSExplicitMessageIDWithMismatchedSCTSIsRejected(t *testing.T) {
	store, _, now := newOutgoingSMS(t)
	_, _, err := store.ApplySMSProtocolEvent(SMSProtocolEvent{
		Phase: "submitted", MessageID: "sms-one", Instance: "engine-1", Destination: "+34600000000",
		Part: 1, Parts: 1, TPReference: smsInt(7),
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	// A plausible past service-centre time remains authoritative and must not
	// be replaced by the event receive time merely because it mismatches.
	wrongTime := now.Add(-time.Hour)
	_, changed, err := store.ApplySMSProtocolEvent(SMSProtocolEvent{
		Phase: "status_report", MessageID: "sms-one", Instance: "engine-1", Destination: "+34600000000",
		Part: 1, Parts: 1, TPReference: smsInt(7), Status: smsInt(0), ServiceCentreTime: &wrongTime,
	}, now.Add(time.Second))
	if !errors.Is(err, ErrSMSReportNotCorrelated) || changed {
		t.Fatalf("changed=%t err=%v", changed, err)
	}
}

func TestSMSStatusWithSCTSWaitsForSubmittedMapping(t *testing.T) {
	store, _, now := newOutgoingSMS(t)
	reportTime := now.Add(time.Second)
	event := SMSProtocolEvent{
		Phase: "status_report", MessageID: "sms-one", Instance: "engine-1", Destination: "+34600000000",
		Part: 1, Parts: 1, TPReference: smsInt(7), Status: smsInt(0), ServiceCentreTime: &reportTime,
	}
	_, changed, err := store.ApplySMSProtocolEvent(event, reportTime)
	if !errors.Is(err, ErrSMSReportNotCorrelated) || changed {
		t.Fatalf("premature status changed=%t err=%v", changed, err)
	}
	submitSMSPart(t, store, "sms-one", "engine-1", "+34600000000", 1, 1, 6, 7, now)
	message, changed, err := store.ApplySMSProtocolEvent(event, reportTime)
	if err != nil || !changed || message.State != "delivered" {
		t.Fatalf("message=%+v changed=%t err=%v", message, changed, err)
	}
}

func TestSMSRestartRejectsStaleRPACKMislabelledAsNewMessage(t *testing.T) {
	store, original, now := newOutgoingSMS(t)
	for index, id := range []string{"sms-old", "sms-new"} {
		message := original
		message.ID = id
		message.Parts = []SMSPartStatus{{
			Part: 1, Parts: 1, Instance: "engine-1", Recipient: message.To,
			RPReference: smsInt(1), TPReference: smsInt(index), State: "submitted", SubmittedAt: &now, UpdatedAt: now,
		}}
		if err := store.SaveMessage(message); err != nil {
			t.Fatal(err)
		}
	}
	_, changed, err := store.ApplySMSProtocolEvent(SMSProtocolEvent{
		Phase: "rp_ack", MessageID: "sms-new", Instance: "engine-1", Destination: original.To,
		Part: 1, Parts: 1, RPReference: smsInt(1), TPReference: smsInt(1),
	}, now.Add(time.Second))
	if !errors.Is(err, ErrSMSReportAmbiguous) || changed {
		t.Fatalf("changed=%t err=%v", changed, err)
	}
}

func TestSMSRestartDoesNotTrustInferredRPDestination(t *testing.T) {
	store, original, now := newOutgoingSMS(t)
	old := original
	old.ID = "sms-old"
	old.To = "+34600000000"
	old.Parts = []SMSPartStatus{{
		Part: 1, Parts: 1, Instance: "engine-1", Recipient: old.To,
		RPReference: smsInt(1), TPReference: smsInt(0), State: "submitted", SubmittedAt: &now, UpdatedAt: now,
	}}
	newer := original
	newer.ID = "sms-new"
	newer.To = "+34999999999"
	newer.Parts = []SMSPartStatus{{
		Part: 1, Parts: 1, Instance: "engine-1", Recipient: newer.To,
		RPReference: smsInt(1), TPReference: smsInt(1), State: "submitted", SubmittedAt: &now, UpdatedAt: now,
	}}
	if err := store.SaveMessage(old); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveMessage(newer); err != nil {
		t.Fatal(err)
	}
	_, changed, err := store.ApplySMSProtocolEvent(SMSProtocolEvent{
		Phase: "rp_ack", MessageID: "sms-new", Instance: "engine-1", Destination: newer.To,
		Part: 1, Parts: 1, RPReference: smsInt(1), TPReference: smsInt(1),
	}, now.Add(time.Second))
	if !errors.Is(err, ErrSMSReportAmbiguous) || changed {
		t.Fatalf("inferred destination hid reused RP reference: changed=%t err=%v", changed, err)
	}
}

func TestSMSRestartCorrelatesRPACKWithoutVolatileTrack(t *testing.T) {
	store, _, now := newOutgoingSMS(t)
	_, _, err := store.ApplySMSProtocolEvent(SMSProtocolEvent{
		Phase: "submitted", MessageID: "sms-one", Instance: "engine-1", Destination: "+34600000000",
		Part: 1, Parts: 1, RPReference: smsInt(1), TPReference: smsInt(0),
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	message, changed, err := store.ApplySMSProtocolEvent(SMSProtocolEvent{
		Phase: "rp_ack", Instance: "engine-1", Part: 1, Parts: 1, RPReference: smsInt(1),
	}, now.Add(time.Second))
	if err != nil || !changed || message.ID != "sms-one" || message.State != "accepted" {
		t.Fatalf("message=%+v changed=%t err=%v", message, changed, err)
	}
}

func TestSMSRestartUsesRecordedTimeToRejectStaleVolatileRPMessageID(t *testing.T) {
	store, original, now := newOutgoingSMS(t)
	oldSubmitted := now
	newSubmitted := now.Add(time.Hour)
	for _, fixture := range []struct {
		id        string
		submitted *time.Time
	}{{"sms-old", &oldSubmitted}, {"sms-new", &newSubmitted}} {
		message := original
		message.ID = fixture.id
		message.Parts = []SMSPartStatus{{
			Part: 1, Parts: 1, Instance: "engine-1", Recipient: message.To,
			RPReference: smsInt(1), TPReference: smsInt(0), State: "submitted",
			SubmittedAt: fixture.submitted, UpdatedAt: *fixture.submitted,
		}}
		if err := store.SaveMessage(message); err != nil {
			t.Fatal(err)
		}
	}
	message, changed, err := store.ApplySMSProtocolEvent(SMSProtocolEvent{
		Phase: "rp_ack", MessageID: "sms-new", Instance: "engine-1", Destination: original.To,
		Part: 1, Parts: 1, RPReference: smsInt(1), TPReference: smsInt(0),
	}, oldSubmitted.Add(time.Second))
	if err != nil || !changed || message.ID != "sms-old" || message.State != "accepted" {
		t.Fatalf("message=%+v changed=%t err=%v", message, changed, err)
	}
}

func TestSMSResponseDoesNotCrossReferenceTypesOrEngineInstances(t *testing.T) {
	store, _, now := newOutgoingSMS(t)
	submitSMSPart(t, store, "sms-one", "engine-1", "+34600000000", 1, 1, 4, 9, now)
	for name, event := range map[string]SMSProtocolEvent{
		"RP must not match TP": {
			Phase: "rp_ack", MessageID: "sms-one", Instance: "engine-1", Destination: "+34600000000",
			Part: 1, Parts: 1, RPReference: smsInt(9),
		},
		"engine instance must match": {
			Phase: "rp_ack", MessageID: "sms-one", Instance: "engine-2", Destination: "+34600000000",
			Part: 1, Parts: 1, RPReference: smsInt(4),
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, changed, err := store.ApplySMSProtocolEvent(event, now.Add(time.Second))
			if !errors.Is(err, ErrSMSReportNotCorrelated) || changed {
				t.Fatalf("changed=%t err=%v", changed, err)
			}
		})
	}
}

func TestSMSMultipartResponseSelectsPartByPhaseReferenceOnly(t *testing.T) {
	store, _, now := newOutgoingSMS(t)
	submitSMSPart(t, store, "sms-one", "engine-1", "+34600000000", 1, 2, 1, 9, now)
	submitSMSPart(t, store, "sms-one", "engine-1", "+34600000000", 2, 2, 2, 10, now.Add(time.Second))
	message, _, err := store.ApplySMSProtocolEvent(SMSProtocolEvent{
		Phase: "rp_ack", MessageID: "sms-one", Instance: "engine-1", Destination: "+34600000000",
		Part: 1, Parts: 2, RPReference: smsInt(2), TPReference: smsInt(9),
	}, now.Add(2*time.Second))
	if err != nil || message.Parts[0].State != "submitted" || message.Parts[1].State != "accepted" {
		t.Fatalf("RP response selected part by auxiliary TP reference: %+v err=%v", message.Parts, err)
	}
	message, _, err = store.ApplySMSProtocolEvent(SMSProtocolEvent{
		Phase: "status_report", MessageID: "sms-one", Instance: "engine-1", Destination: "+34600000000",
		Part: 2, Parts: 2, RPReference: smsInt(2), TPReference: smsInt(9), Status: smsInt(0),
	}, now.Add(3*time.Second))
	if err != nil || message.Parts[0].State != "delivered" || message.Parts[1].State != "accepted" {
		t.Fatalf("status report selected part by auxiliary RP reference: %+v err=%v", message.Parts, err)
	}
}

func TestSMSConflictingSubmittedReferencesAreRejectedAtomically(t *testing.T) {
	store, _, now := newOutgoingSMS(t)
	submitSMSPart(t, store, "sms-one", "engine-1", "+34600000000", 1, 1, 4, 9, now)
	_, changed, err := store.ApplySMSProtocolEvent(SMSProtocolEvent{
		Phase: "submitted", MessageID: "sms-one", Instance: "engine-1", Destination: "+34600000000",
		Part: 1, Parts: 1, RPReference: smsInt(5), TPReference: smsInt(10),
	}, now.Add(time.Second))
	if err == nil || changed {
		t.Fatalf("changed=%t err=%v", changed, err)
	}
	message, _ := store.GetMessage("sms-one")
	if len(message.Parts) != 1 || *message.Parts[0].RPReference != 4 || *message.Parts[0].TPReference != 9 {
		t.Fatalf("conflicting mapping partially mutated message: %+v", message)
	}
}

func TestSMSRestartUsesSCTSToOverrideStaleVolatileMessageID(t *testing.T) {
	store, original, now := newOutgoingSMS(t)
	oldSubmitted := now
	newSubmitted := now.Add(time.Hour)
	for _, fixture := range []struct {
		id        string
		submitted *time.Time
	}{{"sms-old", &oldSubmitted}, {"sms-new", &newSubmitted}} {
		message := original
		message.ID = fixture.id
		message.Parts = []SMSPartStatus{{
			Part: 1, Parts: 1, Instance: "engine-1", Recipient: message.To,
			RPReference: smsInt(1), TPReference: smsInt(0), State: "submitted",
			SubmittedAt: fixture.submitted, UpdatedAt: *fixture.submitted,
		}}
		if err := store.SaveMessage(message); err != nil {
			t.Fatal(err)
		}
	}
	reportTime := oldSubmitted.Add(time.Second)
	message, changed, err := store.ApplySMSProtocolEvent(SMSProtocolEvent{
		Phase: "status_report", MessageID: "sms-new", Instance: "engine-1", Destination: original.To,
		Part: 1, Parts: 1, TPReference: smsInt(0), Status: smsInt(0), ServiceCentreTime: &reportTime,
	}, now.Add(2*time.Hour))
	if err != nil || !changed || message.ID != "sms-old" || message.State != "delivered" {
		t.Fatalf("message=%+v changed=%t err=%v", message, changed, err)
	}
}

func TestSMSFutureSCTSFallsBackToRecordedEventTime(t *testing.T) {
	store, _, now := newOutgoingSMS(t)
	submitSMSPart(t, store, "sms-one", "engine-1", "+34600000000", 1, 1, 4, 9, now)
	receivedAt := now.Add(4 * time.Second)
	misdecodedSCTS := receivedAt.Add(time.Hour)
	message, changed, err := store.ApplySMSProtocolEvent(SMSProtocolEvent{
		Phase: "status_report", Instance: "engine-1", Destination: "+34600000000",
		Part: 1, Parts: 1, TPReference: smsInt(9), Status: smsInt(0), ServiceCentreTime: &misdecodedSCTS,
	}, receivedAt)
	if err != nil || !changed || message.State != "delivered" || message.DeliveryState != "delivered" {
		t.Fatalf("message=%+v changed=%t err=%v", message, changed, err)
	}
}

func TestSMSFutureSCTSFallbackDoesNotGuessReusedReference(t *testing.T) {
	store, original, now := newOutgoingSMS(t)
	for index, id := range []string{"sms-old", "sms-new"} {
		submitted := now.Add(time.Duration(index) * time.Hour)
		message := original
		message.ID = id
		message.Parts = []SMSPartStatus{{
			Part: 1, Parts: 1, Instance: "engine-1", Recipient: message.To,
			TPReference: smsInt(7), State: "submitted", SubmittedAt: &submitted, UpdatedAt: submitted,
		}}
		if err := store.SaveMessage(message); err != nil {
			t.Fatal(err)
		}
	}
	receivedAt := now.Add(2 * time.Hour)
	misdecodedSCTS := receivedAt.Add(time.Hour)
	_, changed, err := store.ApplySMSProtocolEvent(SMSProtocolEvent{
		Phase: "status_report", Instance: "engine-1", Destination: original.To,
		Part: 1, Parts: 1, TPReference: smsInt(7), Status: smsInt(0), ServiceCentreTime: &misdecodedSCTS,
	}, receivedAt)
	if !errors.Is(err, ErrSMSReportAmbiguous) || changed {
		t.Fatalf("changed=%t err=%v", changed, err)
	}
}

func TestSMSSCTSCorrelatesReusedReferenceToOriginalTerminalMessage(t *testing.T) {
	store, original, now := newOutgoingSMS(t)
	oldSubmitted := now.Add(time.Second)
	old := original
	old.ID = "sms-old"
	old.State, old.DeliveryState = "delivered", "delivered"
	old.Parts = []SMSPartStatus{{
		Part: 1, Parts: 1, Instance: "engine-1", Recipient: old.To,
		TPReference: smsInt(7), State: "delivered", SubmittedAt: &oldSubmitted, UpdatedAt: oldSubmitted,
	}}
	newSubmitted := now.Add(time.Hour)
	newer := original
	newer.ID = "sms-new"
	newer.State, newer.DeliveryState = "submitted", "pending"
	newer.Parts = []SMSPartStatus{{
		Part: 1, Parts: 1, Instance: "engine-1", Recipient: newer.To,
		TPReference: smsInt(7), State: "submitted", SubmittedAt: &newSubmitted, UpdatedAt: newSubmitted,
	}}
	if err := store.SaveMessage(old); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveMessage(newer); err != nil {
		t.Fatal(err)
	}
	reportTime := oldSubmitted.Add(20 * time.Second)
	message, changed, err := store.ApplySMSProtocolEvent(SMSProtocolEvent{
		Phase: "status_report", Instance: "engine-1", Destination: old.To,
		Part: 1, Parts: 1, TPReference: smsInt(7), Status: smsInt(0), ServiceCentreTime: &reportTime,
	}, now.Add(2*time.Hour))
	if err != nil || !changed || message.ID != "sms-old" || message.State != "delivered" {
		t.Fatalf("message=%+v changed=%t err=%v", message, changed, err)
	}
	if stored, _ := store.GetMessage("sms-new"); stored.State != "submitted" || stored.DeliveryState != "pending" {
		t.Fatalf("new message was falsely updated: %+v", stored)
	}
}

func TestSMSReusedReferenceWithoutSCTSIsAmbiguousEvenWhenOldMessageTerminal(t *testing.T) {
	store, original, now := newOutgoingSMS(t)
	for index, id := range []string{"sms-old", "sms-new"} {
		message := original
		message.ID = id
		message.State, message.DeliveryState = "submitted", "pending"
		if index == 0 {
			message.State, message.DeliveryState = "delivered", "delivered"
		}
		message.Parts = []SMSPartStatus{{Part: 1, Parts: 1, Recipient: message.To, TPReference: smsInt(7), State: message.State, SubmittedAt: &now, UpdatedAt: now}}
		if err := store.SaveMessage(message); err != nil {
			t.Fatal(err)
		}
	}
	_, changed, err := store.ApplySMSProtocolEvent(SMSProtocolEvent{
		Phase: "status_report", Destination: original.To, Part: 1, Parts: 1,
		TPReference: smsInt(7), Status: smsInt(0),
	}, now.Add(time.Second))
	if !errors.Is(err, ErrSMSReportAmbiguous) || changed {
		t.Fatalf("changed=%t err=%v", changed, err)
	}
}

func TestSMSAmbiguousFallbackDoesNotGuess(t *testing.T) {
	store, original, now := newOutgoingSMS(t)
	for _, id := range []string{"sms-one", "sms-two"} {
		message := original
		message.ID = id
		message.Parts = []SMSPartStatus{{Part: 1, Parts: 1, Recipient: message.To, TPReference: smsInt(8), State: "submitted", SubmittedAt: &now, UpdatedAt: now}}
		if err := store.SaveMessage(message); err != nil {
			t.Fatal(err)
		}
	}
	_, changed, err := store.ApplySMSProtocolEvent(SMSProtocolEvent{
		Phase: "status_report", Destination: original.To, Part: 1, Parts: 1, TPReference: smsInt(8), Status: smsInt(0),
	}, now.Add(time.Second))
	if !errors.Is(err, ErrSMSReportAmbiguous) || changed {
		t.Fatalf("changed=%t err=%v", changed, err)
	}
}

func TestSMSDeliveryExpiryIsUnknownAndLateReportStillWins(t *testing.T) {
	store, _, now := newOutgoingSMS(t)
	deadline := now.Add(time.Minute)
	_, _, err := store.UpdateMessage("sms-one", func(message *Message) bool {
		message.State = "accepted"
		message.DeliveryReportRequested = true
		message.DeliveryReportDeadline = &deadline
		message.Parts = []SMSPartStatus{{Part: 1, Parts: 1, Recipient: message.To, TPReference: smsInt(6), State: "accepted", SubmittedAt: &now, UpdatedAt: now}}
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
	updated, err := store.ExpireSMSDeliveryReports(deadline.Add(time.Second))
	if err != nil || len(updated) != 1 || updated[0].DeliveryState != "unknown" {
		t.Fatalf("updated=%+v err=%v", updated, err)
	}
	message, changed, err := store.ApplySMSProtocolEvent(SMSProtocolEvent{
		Phase: "status_report", Destination: "+34600000000", Part: 1, Parts: 1,
		TPReference: smsInt(6), Status: smsInt(0),
	}, deadline.Add(2*time.Second))
	if err != nil || !changed || message.State != "delivered" {
		t.Fatalf("late report=%+v changed=%t err=%v", message, changed, err)
	}
}

func TestUnsupportedCommandResultIsHonestUnknown(t *testing.T) {
	_, message, now := newOutgoingSMS(t)
	if !applySMSSubmitResult(&message, SMSSubmitResult{State: "submitted", Detail: "local only"}, nil, now.Add(time.Second)) {
		t.Fatal("submission did not change message")
	}
	if message.State != "submitted" || message.DeliveryState != "unknown" || message.StatusDetail != "local only" {
		t.Fatalf("message=%+v", message)
	}
}

func TestVerifiedCommandDeliveryPromotesEarlierCarrierAcceptance(t *testing.T) {
	_, message, now := newOutgoingSMS(t)
	message.State = "accepted"
	message.DeliveryState = "pending"
	if !applySMSSubmitResult(&message, SMSSubmitResult{
		State: "delivered", DeliveryReportExpected: true,
		Detail: "The adapter verified delivery to the destination device.",
	}, nil, now.Add(time.Second)) {
		t.Fatal("verified delivery did not change message")
	}
	if message.State != "delivered" || message.DeliveryState != "delivered" || message.DeliveryReportDeadline != nil {
		t.Fatalf("message=%+v", message)
	}
}

func TestSMSMultipartIsNotAcceptedUntilEverySegmentHasCarrierEvidence(t *testing.T) {
	store, _, now := newOutgoingSMS(t)
	submitSMSPart(t, store, "sms-one", "", "+34600000000", 1, 2, 1, 11, now)
	submitSMSPart(t, store, "sms-one", "", "+34600000000", 2, 2, 2, 12, now.Add(time.Second))
	message, _, err := store.ApplySMSProtocolEvent(SMSProtocolEvent{
		Phase: "rp_ack", MessageID: "sms-one", Destination: "+34600000000",
		Part: 1, Parts: 2, RPReference: smsInt(1), TPReference: smsInt(11),
	}, now.Add(2*time.Second))
	if err != nil || message.State != "submitted" || message.DeliveryState != "pending" {
		t.Fatalf("partial acknowledgement overclaimed acceptance: %+v err=%v", message, err)
	}
	message, _, err = store.ApplySMSProtocolEvent(SMSProtocolEvent{
		Phase: "rp_ack", MessageID: "sms-one", Destination: "+34600000000",
		Part: 2, Parts: 2, RPReference: smsInt(2), TPReference: smsInt(12),
	}, now.Add(3*time.Second))
	if err != nil || message.State != "accepted" || message.DeliveryState != "pending" {
		t.Fatalf("complete acknowledgement was not aggregated: %+v err=%v", message, err)
	}
}

func TestPendingMessageWithoutDeadlineIsReconciledAfterRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().Add(-smsDeliveryReportWindow - time.Minute)
	if err := store.SaveMessage(Message{
		ID: "interrupted", Direction: DirectionOutgoing, To: "+34600000000", Text: "hello",
		State: "sending", DeliveryState: "pending", CreatedAt: old, UpdatedAt: old,
	}); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	message, ok := reopened.GetMessage("interrupted")
	if !ok || message.State != "unknown" || message.DeliveryState != "unknown" || message.StatusDetail == "" {
		t.Fatalf("interrupted message remained pending: %+v", message)
	}
}

func TestRecentSendingMessageIsImmediatelyUnknownAfterRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	deadline := now.Add(smsDeliveryReportWindow)
	if err := store.SaveMessage(Message{
		ID: "interrupted", Direction: DirectionOutgoing, To: "+34600000000", Text: "hello",
		State: "sending", DeliveryState: "pending", DeliveryReportDeadline: &deadline,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	message, ok := reopened.GetMessage("interrupted")
	if !ok || message.State != "unknown" || message.DeliveryState != "unknown" || message.DeliveryReportDeadline != nil {
		t.Fatalf("recent interrupted message remained pending: %+v", message)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var persisted diskState
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatal(err)
	}
	if len(persisted.Messages) != 1 || persisted.Messages[0].State != "unknown" || persisted.Messages[0].DeliveryState != "unknown" {
		t.Fatalf("restart reconciliation was not persisted: %+v", persisted.Messages)
	}
}
