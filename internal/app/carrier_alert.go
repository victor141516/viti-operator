package app

import (
	"context"
	"time"
)

const (
	carrierOutagePollInterval = 10 * time.Second
	carrierOutageGracePeriod  = 45 * time.Second
	carrierAlertRetryInterval = 15 * time.Second
)

type carrierAlertState struct {
	outageSince time.Time
	lastAttempt time.Time
	alerted     bool
	grace       time.Duration
	retry       time.Duration
}

func newCarrierAlertState(grace, retry time.Duration) carrierAlertState {
	return carrierAlertState{grace: grace, retry: retry}
}

func (s *carrierAlertState) shouldAlert(now time.Time, carrierReady, callOpen bool) bool {
	if carrierReady {
		*s = newCarrierAlertState(s.grace, s.retry)
		return false
	}
	if s.outageSince.IsZero() {
		s.outageSince = now
		return false
	}
	if s.alerted || callOpen || now.Sub(s.outageSince) < s.grace {
		return false
	}
	if !s.lastAttempt.IsZero() && now.Sub(s.lastAttempt) < s.retry {
		return false
	}
	s.lastAttempt = now
	return true
}

func (s *carrierAlertState) markAlerted() {
	s.alerted = true
}

// StartCarrierOutageNotifier watches the live carrier path: SWu, IMS
// registration and the local Asterisk control plane must all be ready. SIM
// readiness is intentionally excluded because an already-established tunnel
// can remain usable while its AKA provider is temporarily unavailable. An
// intentionally disabled IMS registration is not an outage. One local SIP
// alert is emitted per sustained outage, after a grace period, and is deferred
// while a real call is open.
func (a *App) StartCarrierOutageNotifier(ctx context.Context) {
	notifier, ok := a.gateway.(CarrierOutageNotifier)
	if !ok {
		return
	}
	go func() {
		state := newCarrierAlertState(carrierOutageGracePeriod, carrierAlertRetryInterval)
		ticker := time.NewTicker(carrierOutagePollInterval)
		defer ticker.Stop()
		for {
			statusCtx, cancel := context.WithTimeout(ctx, 7*time.Second)
			status, _ := a.runtimeStatus(statusCtx)
			cancel()
			registrationEnabled := a.store.IMSRegistrationEnabled()
			carrierReady := !registrationEnabled || establishedCarrierPathReady(status)
			if state.shouldAlert(time.Now().UTC(), carrierReady, a.hasOpenCall()) {
				alertCtx, alertCancel := context.WithTimeout(ctx, 8*time.Second)
				err := notifier.NotifyCarrierOutage(alertCtx)
				alertCancel()
				if err != nil {
					a.logger.Warn("carrier outage alert failed; retrying", "error", err)
					a.diag.RecordEvent("manager.runtime", "carrier-outage-alert.failed", map[string]any{"error": err.Error()})
				} else {
					state.markAlerted()
					a.logger.Warn("carrier outage alert sent")
					a.diag.RecordEvent("manager.runtime", "carrier-outage-alert.sent", map[string]any{"outage_since": state.outageSince})
					a.events.Publish("carrier.outage.alerted", map[string]any{"state": "down", "label": "Viti Server down", "outage_since": state.outageSince})
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

func (a *App) hasOpenCall() bool {
	for _, call := range a.store.Calls() {
		if call.EndedAt == nil {
			return true
		}
	}
	return false
}
