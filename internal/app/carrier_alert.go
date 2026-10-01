package app

import (
	"context"
	"time"
)

const (
	carrierOutagePollInterval = 10 * time.Second
	carrierOutageGracePeriod  = 45 * time.Second
	// Longer than the bounded 45-second local alert leg: never stack calls.
	carrierAlertRetryInterval = 60 * time.Second
)

type carrierAlertState struct {
	outageSince time.Time
	lastAttempt time.Time
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
	if callOpen || now.Sub(s.outageSince) < s.grace {
		return false
	}
	if !s.lastAttempt.IsZero() && now.Sub(s.lastAttempt) < s.retry {
		return false
	}
	s.lastAttempt = now
	return true
}

// StartCarrierOutageNotifier watches the live carrier path: SWu, IMS
// registration and the local Asterisk control plane must all be ready. SIM
// readiness is intentionally excluded because an already-established tunnel
// can remain usable while its AKA provider is temporarily unavailable. An
// intentionally disabled IMS registration is not an outage. Local SIP alerts
// repeat throughout a sustained outage, including for clients that reconnect
// later. Recovery or a real call cancels only the service-alert legs.
func (a *App) StartCarrierOutageNotifier(ctx context.Context) {
	notifier, ok := a.gateway.(CarrierOutageNotifier)
	if !ok {
		return
	}
	go func() {
		state := newCarrierAlertState(carrierOutageGracePeriod, carrierAlertRetryInterval)
		// Also clean up an alert left by a previous manager process.
		clearNeeded := true
		ticker := time.NewTicker(carrierOutagePollInterval)
		defer ticker.Stop()
		for {
			statusCtx, cancel := context.WithTimeout(ctx, 7*time.Second)
			status, _ := a.runtimeStatus(statusCtx)
			cancel()
			registrationEnabled := a.store.IMSRegistrationEnabled()
			carrierReady := !registrationEnabled || establishedCarrierPathReady(status)
			callOpen := a.hasOpenCall()
			if clearNeeded && (carrierReady || callOpen) {
				clearCtx, clearCancel := context.WithTimeout(ctx, 8*time.Second)
				err := notifier.CancelCarrierOutage(clearCtx)
				clearCancel()
				if err == nil {
					clearNeeded = false
				} else {
					a.logger.Warn("could not clear carrier outage alert; retrying", "error", err)
				}
			}
			if state.shouldAlert(time.Now().UTC(), carrierReady, callOpen) {
				clearNeeded = true
				alertCtx, alertCancel := context.WithTimeout(ctx, 8*time.Second)
				err := notifier.NotifyCarrierOutage(alertCtx)
				alertCancel()
				if err != nil {
					a.logger.Warn("carrier outage alert failed; retrying", "error", err)
					a.diag.RecordEvent("manager.runtime", "carrier-outage-alert.failed", map[string]any{"error": err.Error()})
				} else {
					a.logger.Warn("carrier outage alert requested; repeating until recovery")
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
