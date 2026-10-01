package app

import (
	"testing"
	"time"
)

func TestCarrierAlertStateRepeatsThroughoutSustainedOutage(t *testing.T) {
	start := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	state := newCarrierAlertState(45*time.Second, 15*time.Second)
	if state.shouldAlert(start, false, false) {
		t.Fatal("new outage alerted without a grace period")
	}
	if state.shouldAlert(start.Add(44*time.Second), false, false) {
		t.Fatal("outage alerted before the grace period")
	}
	if !state.shouldAlert(start.Add(45*time.Second), false, false) {
		t.Fatal("sustained outage did not alert")
	}
	if state.shouldAlert(start.Add(59*time.Second), false, false) {
		t.Fatal("alert repeated before the retry interval")
	}
	if !state.shouldAlert(start.Add(60*time.Second), false, false) {
		t.Fatal("same outage did not alert again for late or reconnected clients")
	}
	if !state.shouldAlert(start.Add(time.Hour), false, false) {
		t.Fatal("persistent outage stopped alerting")
	}
}

func TestCarrierAlertStateDefersCallsAndRetriesFailures(t *testing.T) {
	start := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	state := newCarrierAlertState(5*time.Second, 15*time.Second)
	state.shouldAlert(start, false, false)
	if state.shouldAlert(start.Add(5*time.Second), false, true) {
		t.Fatal("alert interrupted an open call")
	}
	if !state.shouldAlert(start.Add(6*time.Second), false, false) {
		t.Fatal("alert was not attempted after the call ended")
	}
	if state.shouldAlert(start.Add(20*time.Second), false, false) {
		t.Fatal("failed alert retried too soon")
	}
	if !state.shouldAlert(start.Add(21*time.Second), false, false) {
		t.Fatal("failed alert was not retried")
	}
}

func TestCarrierAlertStateRecoveryStartsANewOutage(t *testing.T) {
	start := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	state := newCarrierAlertState(time.Second, time.Second)
	state.shouldAlert(start, false, false)
	if !state.shouldAlert(start.Add(time.Second), false, false) {
		t.Fatal("first outage did not alert")
	}
	if state.shouldAlert(start.Add(2*time.Second), true, false) {
		t.Fatal("recovery emitted an alert")
	}
	if state.shouldAlert(start.Add(3*time.Second), false, false) {
		t.Fatal("new outage did not receive a fresh grace period")
	}
	if !state.shouldAlert(start.Add(4*time.Second), false, false) {
		t.Fatal("new sustained outage did not alert")
	}
}

func TestCarrierServiceReadyIgnoresTemporarySIMAuthenticatorLoss(t *testing.T) {
	status := RuntimeStatus{SIMReady: false, TunnelReady: true, IMSReady: true}
	if !establishedCarrierPathReady(status) {
		t.Fatal("an established carrier path was treated as down only because SIM authentication was unavailable")
	}
	status.TunnelReady = false
	if establishedCarrierPathReady(status) {
		t.Fatal("a down carrier tunnel was treated as ready")
	}
}
