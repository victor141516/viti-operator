package app

import (
	"context"
	"errors"
	"strings"
	"time"
)

type SIMAuthAssignment struct {
	ClientID  string    `json:"client_id,omitempty"`
	AgentID   string    `json:"agent_id,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
}

type SIMAuthSelection struct {
	Mode       string
	ClientID   string
	ClientName string
	AgentID    string
}

type AdaptiveAuthenticator struct {
	Broker        *AuthBroker
	Fallback      SIMAuthenticator
	FallbackMode  string
	RemoteTimeout time.Duration
	Select        func(time.Time) SIMAuthSelection
}

func (a *AdaptiveAuthenticator) selection() SIMAuthSelection {
	if a.Select == nil {
		return SIMAuthSelection{Mode: a.FallbackMode}
	}
	selection := a.Select(time.Now().UTC())
	if selection.Mode == "" {
		selection.Mode = a.FallbackMode
	}
	return selection
}

func (a *AdaptiveAuthenticator) AuthenticateAKA(ctx context.Context, rand16, autn16 []byte) (AKAResult, error) {
	selection := a.selection()
	if selection.Mode == "remote" {
		return RemoteAuthenticator{Broker: a.Broker, AgentID: selection.AgentID, Timeout: a.remoteTimeout()}.
			AuthenticateAKA(ctx, rand16, autn16)
	}
	if a.Fallback != nil {
		return a.Fallback.AuthenticateAKA(ctx, rand16, autn16)
	}
	return AKAResult{}, errors.New("no device is available to authenticate with the SIM")
}

func (a *AdaptiveAuthenticator) CheckSIMReady(ctx context.Context) error {
	selection := a.selection()
	if selection.Mode == "remote" && strings.TrimSpace(selection.AgentID) != "" {
		return nil
	}
	if checker, ok := a.Fallback.(SIMReadinessChecker); ok {
		return checker.CheckSIMReady(ctx)
	}
	if a.Fallback != nil {
		return nil
	}
	return errors.New("no authentication phone or USB modem is available")
}

func (a *AdaptiveAuthenticator) remoteTimeout() time.Duration {
	if a.RemoteTimeout > 0 {
		return a.RemoteTimeout
	}
	return 35 * time.Second
}
