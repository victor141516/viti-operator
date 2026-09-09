package app

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"
)

type AKAResult struct {
	RES  []byte
	CK   []byte
	IK   []byte
	AUTS []byte
}

type SIMAuthenticator interface {
	AuthenticateAKA(context.Context, []byte, []byte) (AKAResult, error)
}

// SIMReadinessChecker is implemented by authenticators backed by removable
// hardware. It lets runtime status distinguish a live IMS registration from a
// stale one left behind immediately after unplugging the device.
type SIMReadinessChecker interface {
	CheckSIMReady(context.Context) error
}

type AuthChallenge struct {
	ID        string    `json:"id"`
	AgentID   string    `json:"agent_id"`
	RAND      string    `json:"rand"`
	AUTN      string    `json:"autn"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

type AuthResponse struct {
	RES   string `json:"res,omitempty"`
	CK    string `json:"ck,omitempty"`
	IK    string `json:"ik,omitempty"`
	AUTS  string `json:"auts,omitempty"`
	Error string `json:"error,omitempty"`
}

type pendingAuth struct {
	challenge AuthChallenge
	done      chan struct{}
	response  AuthResponse
	answered  bool
}

type AuthBroker struct {
	mu      sync.Mutex
	pending map[string]*pendingAuth
	waiters map[string]map[chan struct{}]struct{}
}

func NewAuthBroker() *AuthBroker {
	return &AuthBroker{pending: map[string]*pendingAuth{}, waiters: map[string]map[chan struct{}]struct{}{}}
}

func (b *AuthBroker) Submit(ctx context.Context, agentID string, rand16, autn16 []byte, timeout time.Duration) (AKAResult, error) {
	if len(rand16) != 16 || len(autn16) != 16 {
		return AKAResult{}, errors.New("RAND and AUTN must be 16 bytes")
	}
	id, err := randomID("aka")
	if err != nil {
		return AKAResult{}, err
	}
	now := time.Now().UTC()
	item := &pendingAuth{
		challenge: AuthChallenge{ID: id, AgentID: agentID, RAND: base64.RawStdEncoding.EncodeToString(rand16), AUTN: base64.RawStdEncoding.EncodeToString(autn16), CreatedAt: now, ExpiresAt: now.Add(timeout)},
		done:      make(chan struct{}),
	}
	b.mu.Lock()
	b.pending[id] = item
	b.notifyAgentLocked(agentID)
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		delete(b.pending, id)
		b.mu.Unlock()
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return AKAResult{}, ctx.Err()
	case <-timer.C:
		return AKAResult{}, errors.New("remote SIM authentication timed out")
	case <-item.done:
	}
	if item.response.Error != "" {
		return AKAResult{}, errors.New(item.response.Error)
	}
	return decodeAKAResponse(item.response)
}

func (b *AuthBroker) Poll(ctx context.Context, agentID string) *AuthChallenge {
	for {
		b.mu.Lock()
		for _, item := range b.pending {
			if item.challenge.AgentID == agentID && !item.answered && time.Now().Before(item.challenge.ExpiresAt) {
				challenge := item.challenge
				b.mu.Unlock()
				return &challenge
			}
		}
		waiter := make(chan struct{})
		if b.waiters[agentID] == nil {
			b.waiters[agentID] = map[chan struct{}]struct{}{}
		}
		b.waiters[agentID][waiter] = struct{}{}
		b.mu.Unlock()
		select {
		case <-ctx.Done():
			b.mu.Lock()
			delete(b.waiters[agentID], waiter)
			b.mu.Unlock()
			return nil
		case <-waiter:
		}
	}
}

func (b *AuthBroker) Respond(agentID, challengeID string, response AuthResponse) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	item := b.pending[challengeID]
	if item == nil || item.challenge.AgentID != agentID {
		return errors.New("challenge not found")
	}
	if time.Now().After(item.challenge.ExpiresAt) {
		return errors.New("challenge expired")
	}
	if item.answered {
		return errors.New("challenge already answered")
	}
	if response.Error == "" {
		if _, err := decodeAKAResponse(response); err != nil {
			return err
		}
	}
	item.response = response
	item.answered = true
	close(item.done)
	return nil
}

func (b *AuthBroker) notifyAgentLocked(agentID string) {
	for waiter := range b.waiters[agentID] {
		close(waiter)
		delete(b.waiters[agentID], waiter)
	}
}

type RemoteAuthenticator struct {
	Broker  *AuthBroker
	AgentID string
	Timeout time.Duration
}

func (a RemoteAuthenticator) AuthenticateAKA(ctx context.Context, rand16, autn16 []byte) (AKAResult, error) {
	return a.Broker.Submit(ctx, a.AgentID, rand16, autn16, a.Timeout)
}

func decodeAKAResponse(response AuthResponse) (AKAResult, error) {
	decode := func(name, value string, requiredLength int) ([]byte, error) {
		if value == "" {
			return nil, nil
		}
		data, err := base64.RawStdEncoding.DecodeString(value)
		if err != nil {
			return nil, fmt.Errorf("invalid %s", name)
		}
		if requiredLength > 0 && len(data) != requiredLength {
			return nil, fmt.Errorf("invalid %s length", name)
		}
		return data, nil
	}
	res, err := decode("RES", response.RES, 0)
	if err != nil {
		return AKAResult{}, err
	}
	ck, err := decode("CK", response.CK, 16)
	if err != nil {
		return AKAResult{}, err
	}
	ik, err := decode("IK", response.IK, 16)
	if err != nil {
		return AKAResult{}, err
	}
	auts, err := decode("AUTS", response.AUTS, 14)
	if err != nil {
		return AKAResult{}, err
	}
	if len(auts) == 14 && len(res) == 0 && len(ck) == 0 && len(ik) == 0 {
		return AKAResult{AUTS: auts}, nil
	}
	if len(res) < 4 || len(res) > 16 || len(ck) != 16 || len(ik) != 16 || len(auts) != 0 {
		return AKAResult{}, errors.New("AKA response must contain RES/CK/IK or AUTS")
	}
	return AKAResult{RES: res, CK: ck, IK: ik}, nil
}

func randomID(prefix string) (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return prefix + "-" + hex.EncodeToString(value[:]), nil
}

func shortHash(value string) []byte {
	var out [4]byte
	for index := range value {
		out[index%len(out)] = out[index%len(out)]*33 ^ value[index]
	}
	return out[:]
}
