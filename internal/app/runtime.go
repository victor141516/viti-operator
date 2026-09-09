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
	Seen      bool
	Ready     bool
	Detail    string
	UpdatedAt time.Time
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

func (a *App) setEngineTunnelState(ready bool, args []string, recordedAtNS int64) {
	updatedAt := time.Now().UTC()
	if recordedAtNS > 0 {
		updatedAt = time.Unix(0, recordedAtNS).UTC()
	}
	detail := strings.TrimSpace(strings.Join(args, " "))
	a.stateMu.Lock()
	previous := a.tunnel
	a.tunnel = engineTunnelState{Seen: true, Ready: ready, Detail: detail, UpdatedAt: updatedAt}
	current := a.tunnel
	a.stateMu.Unlock()
	a.diag.RecordEvent("manager.runtime", "tunnel-state.changed", map[string]any{"previous": previous, "current": current})
	if !ready {
		calls, err := a.store.InterruptOpenCalls(updatedAt)
		if err != nil {
			a.diag.RecordEvent("manager.runtime", "calls.interrupt-failed", map[string]any{"error": err.Error()})
		} else {
			for _, call := range calls {
				a.events.Publish("call.updated", call)
			}
		}
	}
	a.events.Publish("status.updated", current)
}

func (a *App) runtimeStatus(ctx context.Context) (RuntimeStatus, error) {
	status, gatewayErr := a.gateway.Status(ctx)
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
			status.TunnelReady = false
			status.IMSReady = false
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
	status.Ready = registrationEnabled && status.SIMReady && status.TunnelReady && status.IMSReady
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
	if !status.SIMReady {
		return "No authentication phone or USB modem with a SIM is available"
	}
	if !status.TunnelReady {
		return "The modem is connected, but the network tunnel is not ready yet"
	}
	return "IMS registration is not ready yet"
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
