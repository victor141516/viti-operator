package app

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

var errRuntimeUnavailable = errors.New("telephony runtime is unavailable")

type engineTunnelState struct {
	Seen       bool
	Ready      bool
	Detail     string
	UpdatedAt  time.Time
	Instance   string
	Generation string
	Sequence   uint64
}

type EngineTunnelCheckpoint struct {
	Instance           string    `json:"instance"`
	Generation         string    `json:"generation"`
	Sequence           uint64    `json:"sequence"`
	Ready              bool      `json:"ready"`
	UpdatedAt          time.Time `json:"updated_at"`
	LastDownAt         time.Time `json:"last_down_at,omitempty"`
	RetiredGenerations []string  `json:"retired_generations,omitempty"`
}

func advanceEngineTunnelCheckpoint(previous EngineTunnelCheckpoint, instance, generation string, sequence uint64, ready, claimsGeneration bool, at time.Time) (EngineTunnelCheckpoint, bool) {
	if instance == "" || generation == "" || sequence == 0 || at.IsZero() {
		return previous, false
	}
	if previous.Generation == generation {
		if sequence <= previous.Sequence {
			return previous, false
		}
		if !ready {
			previous.LastDownAt = at
		}
		previous.Sequence, previous.Ready, previous.UpdatedAt = sequence, ready, at
		return previous, true
	}
	for _, retired := range previous.RetiredGenerations {
		if retired == generation {
			return previous, false
		}
	}
	// Generations share the engine host's clock. An unobserved older process can
	// be retried from the durable outbox after a newer process has already taken
	// ownership; it must not retire that newer generation.
	if previous.Generation != "" && !at.After(previous.UpdatedAt) {
		return previous, false
	}
	// Only a durable process-start or tunnel-up event can transfer ownership to
	// a generation that the manager has never observed. A standalone down event
	// cannot prove that it belongs to the currently established data plane.
	if previous.Generation != "" && !claimsGeneration {
		return previous, false
	}
	retired := append([]string(nil), previous.RetiredGenerations...)
	if previous.Generation != "" {
		retired = append(retired, previous.Generation)
	}
	lastDownAt := previous.LastDownAt
	if !ready {
		lastDownAt = at
	}
	return EngineTunnelCheckpoint{
		Instance: instance, Generation: generation, Sequence: sequence,
		Ready: ready, UpdatedAt: at, LastDownAt: lastDownAt, RetiredGenerations: retired,
	}, true
}

// StartIMSPreferenceReconciler keeps an intentionally disabled registration
// disabled across Asterisk starts, P-CSCF changes and tunnel recovery. Those
// events can independently trigger `pjsip send register` in the engine.
func (a *App) StartIMSPreferenceReconciler(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			a.enforceDisabledIMSRegistration(ctx)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

// StartSMSDeliveryReconciler converts an unanswered delivery-report request to
// an explicit unknown state. Reports arriving later still update that state.
func (a *App) StartSMSDeliveryReconciler(ctx context.Context) {
	reconcile := func() {
		messages, err := a.store.ExpireSMSDeliveryReports(time.Now().UTC())
		if err != nil {
			a.logger.Warn("expire SMS delivery reports", "error", err)
			return
		}
		for _, message := range messages {
			a.events.Publish("message.updated", message)
		}
	}
	reconcile()
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				reconcile()
			}
		}
	}()
}

func (a *App) enforceDisabledIMSRegistration(parent context.Context) {
	if a.store.IMSRegistrationEnabled() {
		return
	}
	controller, ok := a.gateway.(IMSRegistrationController)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(parent, 4*time.Second)
	defer cancel()
	status, _ := a.gateway.Status(ctx)
	if !status.IMSReady {
		return
	}
	a.imsMu.Lock()
	err := controller.SetIMSRegistration(ctx, false)
	a.imsMu.Unlock()
	if err != nil {
		a.diag.RecordEvent("manager.ims", "registration-disable.enforce-failed", map[string]any{"error": err.Error()})
		return
	}
	a.diag.RecordEvent("manager.ims", "registration-disable.enforced", nil)
}

func (a *App) ingestEngineSMS(w http.ResponseWriter, instance string, args []string, recordedAtNS int64) {
	if len(args) < 2 || strings.TrimSpace(args[0]) == "" {
		writeError(w, http.StatusBadRequest, "invalid incoming SMS event")
		return
	}
	body, err := decodeEngineBase64(args[1])
	if err != nil {
		a.diag.RecordEvent("manager.sms", "incoming.decode-failed", map[string]any{"instance": instance, "recorded_at_ns": recordedAtNS, "error": err.Error()})
		writeError(w, http.StatusBadRequest, "incoming SMS body is not valid Base64")
		return
	}
	receivedAt := time.Now().UTC()
	if recordedAtNS > 0 {
		receivedAt = time.Unix(0, recordedAtNS).UTC()
	}
	sender := strings.TrimSpace(args[0])
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%s", instance, sender, body)))
	messageID := fmt.Sprintf("sms-%d-%x", recordedAtNS, digest[:8])
	if existing, ok := a.store.GetMessage(messageID); ok {
		a.diag.RecordEvent("manager.sms", "incoming.duplicate", map[string]any{"instance": instance, "message_id": messageID, "recorded_at_ns": recordedAtNS})
		writeJSON(w, http.StatusOK, existing)
		return
	}
	message := Message{
		ID: messageID, Direction: DirectionIncoming, From: sender,
		Text: string(body), State: "received", CreatedAt: receivedAt, UpdatedAt: receivedAt,
	}
	if err := a.store.SaveMessage(message); err != nil {
		a.diag.RecordEvent("manager.sms", "incoming.persist-failed", map[string]any{"instance": instance, "message_id": messageID, "error": err.Error()})
		writeError(w, http.StatusInternalServerError, "could not persist incoming SMS")
		return
	}
	a.events.Publish("message.received", message)
	a.diag.RecordEvent("manager.sms", "incoming.persisted", map[string]any{"instance": instance, "message_id": messageID, "sender": sender, "recorded_at_ns": recordedAtNS, "body_bytes": len(body)})
	writeJSON(w, http.StatusOK, message)
}

func decodeEngineBase64(value string) ([]byte, error) {
	for _, encoding := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding} {
		if decoded, err := encoding.DecodeString(strings.TrimSpace(value)); err == nil {
			return decoded, nil
		}
	}
	return nil, errors.New("invalid Base64")
}

func (a *App) setEngineTunnelState(instance, generation string, sequence uint64, ready, claimsGeneration bool, args []string, recordedAtNS int64) error {
	updatedAt := time.Now().UTC()
	if recordedAtNS > 0 {
		updatedAt = time.Unix(0, recordedAtNS).UTC()
	}
	detail := strings.TrimSpace(strings.Join(args, " "))
	a.stateMu.Lock()
	checkpoint, calls, accepted, err := a.store.ApplyEngineTunnelEvent(instance, generation, sequence, ready, claimsGeneration, updatedAt)
	if err != nil {
		a.stateMu.Unlock()
		a.diag.RecordEvent("manager.runtime", "tunnel-state.persist-failed", map[string]any{"instance": instance, "generation": generation, "sequence": sequence, "error": err.Error()})
		return err
	}
	if !accepted {
		a.stateMu.Unlock()
		a.diag.RecordEvent("manager.runtime", "tunnel-state.ignored", map[string]any{"instance": instance, "generation": generation, "sequence": sequence, "ready": ready})
		return nil
	}
	previous := a.tunnel
	a.tunnel = engineTunnelState{
		Seen: true, Ready: checkpoint.Ready, Detail: detail, UpdatedAt: checkpoint.UpdatedAt,
		Instance: checkpoint.Instance, Generation: checkpoint.Generation, Sequence: checkpoint.Sequence,
	}
	current := a.tunnel
	a.stateMu.Unlock()
	a.diag.RecordEvent("manager.runtime", "tunnel-state.changed", map[string]any{"previous": previous, "current": current})
	for _, call := range calls {
		a.events.Publish("call.updated", call)
	}
	a.events.Publish("status.updated", current)
	return nil
}

func (a *App) runtimeStatus(ctx context.Context) (RuntimeStatus, error) {
	status, gatewayErr := a.gateway.Status(ctx)
	status.ContactsVersion = a.store.ContactsVersion()
	registrationEnabled := a.store.IMSRegistrationEnabled()
	if status.UpdatedAt.IsZero() {
		status.UpdatedAt = time.Now().UTC()
	}
	details := make([]string, 0, 3)
	if !registrationEnabled {
		status.IMSReady = false
		status.Ready = false
		details = append(details, "Server IMS registration is disabled")
		gatewayErr = errors.Join(gatewayErr, errRuntimeUnavailable)
	}
	if strings.TrimSpace(status.Detail) != "" {
		details = append(details, strings.TrimSpace(status.Detail))
	}

	if checker, ok := a.auth.(SIMReadinessChecker); ok {
		healthCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		healthErr := checker.CheckSIMReady(healthCtx)
		cancel()
		if healthErr != nil {
			status.SIMReady = false
			details = append(details, healthErr.Error())
			gatewayErr = errors.Join(gatewayErr, healthErr)
		} else {
			status.SIMReady = true
		}
	}

	a.stateMu.RLock()
	tunnel := a.tunnel
	a.stateMu.RUnlock()
	if tunnel.Seen {
		status.TunnelReady = tunnel.Ready
		if tunnel.Detail != "" {
			details = append(details, tunnel.Detail)
		}
		if !tunnel.Ready {
			status.IMSReady = false
			gatewayErr = errors.Join(gatewayErr, errRuntimeUnavailable)
		}
	}
	// An unavailable AKA provider prevents the next authentication exchange,
	// but it does not invalidate an already-established SWu tunnel or IMS
	// registration. Keep the line usable until the carrier path itself fails.
	status.Ready = registrationEnabled && establishedCarrierPathReady(status)
	status.Detail = strings.Join(uniqueStrings(details), " | ")
	if !status.Ready {
		resultErr := errors.Join(errRuntimeUnavailable, gatewayErr)
		a.diag.RecordEvent("manager.runtime", "status.checked", map[string]any{"status": status, "error": errorString(resultErr), "tunnel_event": tunnel})
		return status, resultErr
	}
	a.diag.RecordEvent("manager.runtime", "status.checked", map[string]any{"status": status, "tunnel_event": tunnel})
	return status, nil
}

func runtimeUnavailableMessage(status RuntimeStatus) string {
	if !status.TunnelReady {
		return "The carrier network tunnel is not ready yet"
	}
	if !status.IMSReady {
		return "IMS registration is not ready yet"
	}
	return "The carrier line is not ready yet"
}

func establishedCarrierPathReady(status RuntimeStatus) bool {
	return status.TunnelReady && status.IMSReady
}

func uniqueStrings(values []string) []string {
	seen := map[string]struct{}{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			if _, ok := seen[value]; ok {
				continue
			}
			seen[value] = struct{}{}
			result = append(result, value)
		}
	}
	return result
}
