package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

type TelephonyGateway interface {
	Status(context.Context) (RuntimeStatus, error)
	SendSMS(context.Context, string, string) (SMSSubmitResult, error)
	StartCall(context.Context, string, string) error
	HangupCall(context.Context, string) error
}

type IMSRegistrationController interface {
	SetIMSRegistration(context.Context, bool) error
}

// SMSSubmitResult describes only evidence the transport actually obtained. It
// deliberately separates local submission, carrier acceptance and verified
// delivery so clients never present a timeout as a delivered message.
type SMSSubmitResult struct {
	State string
}

type IncomingMessageSource interface {
	IncomingMessages(context.Context) ([]Message, error)
}

var ErrDeliveryUnconfirmed = errors.New("SMS submitted but delivery report is not yet available")

type MemoryGateway struct{}

func (MemoryGateway) Status(context.Context) (RuntimeStatus, error) {
	return RuntimeStatus{Ready: true, SIMReady: true, TunnelReady: true, IMSReady: true, Provider: "memory", UpdatedAt: time.Now().UTC()}, nil
}
func (MemoryGateway) SendSMS(context.Context, string, string) (SMSSubmitResult, error) {
	return SMSSubmitResult{State: "accepted"}, nil
}
func (MemoryGateway) StartCall(context.Context, string, string) error { return nil }
func (MemoryGateway) HangupCall(context.Context, string) error        { return nil }
func (MemoryGateway) SetIMSRegistration(context.Context, bool) error  { return nil }

type CommandGateway struct {
	cfg  TelephonyConfig
	mu   sync.Mutex
	seen map[string]struct{}
}

func NewCommandGateway(cfg TelephonyConfig) *CommandGateway {
	return &CommandGateway{cfg: cfg, seen: map[string]struct{}{}}
}

func (g *CommandGateway) Status(ctx context.Context) (RuntimeStatus, error) {
	output, err := runCommand(ctx, g.cfg.Status, nil)
	status := RuntimeStatus{Provider: "command", UpdatedAt: time.Now().UTC()}
	if err != nil {
		status.Detail = err.Error()
		return status, err
	}
	text := strings.TrimSpace(string(output))
	status.Detail = text
	status.Ready = strings.Contains(strings.ToLower(text), "ready") || strings.Contains(text, "Registered")
	status.SIMReady, status.TunnelReady, status.IMSReady = status.Ready, status.Ready, status.Ready
	return status, nil
}

func (g *CommandGateway) SendSMS(ctx context.Context, to, text string) (SMSSubmitResult, error) {
	spec := expandCommand(g.cfg.SendSMS, map[string]string{"to": to, "text": text})
	output, err := runCommand(ctx, spec, commandInput(spec))
	result := classifySMSSubmitOutput(output)
	if err != nil && (bytes.Contains(output, []byte("SMS_RESULT=submit-report-timeout")) || errors.Is(ctx.Err(), context.DeadlineExceeded)) {
		return SMSSubmitResult{State: "submitted"}, ErrDeliveryUnconfirmed
	}
	if err != nil {
		return result, err
	}
	if result.State == "" {
		// Generic command adapters may not emit Viti's optional result marker. A
		// successful process is still only carrier acceptance, never delivery.
		result.State = "accepted"
	}
	return result, nil
}

func classifySMSSubmitOutput(output []byte) SMSSubmitResult {
	switch {
	case bytes.Contains(output, []byte("SMS_RESULT=delivered-and-verified")):
		return SMSSubmitResult{State: "delivered"}
	case bytes.Contains(output, []byte("SMS_RESULT=network-accepted")):
		return SMSSubmitResult{State: "accepted"}
	case bytes.Contains(output, []byte("SMS_RESULT=submit-report-timeout")):
		return SMSSubmitResult{State: "submitted"}
	case bytes.Contains(output, []byte("SMS_RESULT=operator-barred")),
		bytes.Contains(output, []byte("SMS_RESULT=network-rejected")),
		bytes.Contains(output, []byte("SMS_RESULT=local-submit-failed")):
		return SMSSubmitResult{State: "failed"}
	default:
		return SMSSubmitResult{}
	}
}

func (g *CommandGateway) StartCall(ctx context.Context, callID, to string) error {
	spec := expandCommand(g.cfg.StartCall, map[string]string{"call_id": callID, "to": to})
	_, err := runCommand(ctx, spec, commandInput(spec))
	return err
}

func (g *CommandGateway) HangupCall(ctx context.Context, callID string) error {
	spec := expandCommand(g.cfg.HangupCall, map[string]string{"call_id": callID})
	_, err := runCommand(ctx, spec, commandInput(spec))
	return err
}

func (g *CommandGateway) IncomingMessages(ctx context.Context) ([]Message, error) {
	output, err := runCommand(ctx, g.cfg.IncomingSMS, nil)
	if err != nil {
		return nil, err
	}
	var incoming []Message
	for _, line := range bytes.Split(output, []byte{'\n'}) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var raw struct {
			ID         string `json:"id"`
			ReceivedAt int64  `json:"received_at"`
			From       string `json:"from"`
			To         string `json:"to"`
			Text       string `json:"text"`
		}
		if json.Unmarshal(line, &raw) != nil {
			continue
		}
		if raw.ID == "" {
			raw.ID = fmt.Sprintf("sms-%d-%x", raw.ReceivedAt, shortHash(raw.From+"\x00"+raw.Text))
		}
		g.mu.Lock()
		_, exists := g.seen[raw.ID]
		if !exists {
			g.seen[raw.ID] = struct{}{}
		}
		g.mu.Unlock()
		if exists {
			continue
		}
		at := time.Unix(raw.ReceivedAt, 0).UTC()
		if raw.ReceivedAt == 0 {
			at = time.Now().UTC()
		}
		incoming = append(incoming, Message{ID: raw.ID, Direction: DirectionIncoming, From: raw.From, To: raw.To, Text: raw.Text, State: "received", CreatedAt: at, UpdatedAt: at})
	}
	return incoming, nil
}

func runCommand(ctx context.Context, spec CommandSpec, stdin []byte) ([]byte, error) {
	if strings.TrimSpace(spec.Executable) == "" {
		return nil, errors.New("command is not configured")
	}
	cmd := exec.CommandContext(ctx, spec.Executable, spec.Args...)
	cmd.Env = os.Environ()
	for key, value := range spec.Env {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		return output, fmt.Errorf("%s: %w: %s", spec.Executable, err, strings.TrimSpace(string(output)))
	}
	return output, nil
}

func expandCommand(spec CommandSpec, values map[string]string) CommandSpec {
	out := spec
	out.Args = append([]string(nil), spec.Args...)
	for index, arg := range out.Args {
		for key, value := range values {
			arg = strings.ReplaceAll(arg, "{{"+key+"}}", value)
		}
		out.Args[index] = arg
	}
	for key, value := range values {
		out.Stdin = strings.ReplaceAll(out.Stdin, "{{"+key+"}}", value)
	}
	return out
}

func commandInput(spec CommandSpec) []byte {
	if spec.Stdin == "" {
		return nil
	}
	return []byte(spec.Stdin)
}
