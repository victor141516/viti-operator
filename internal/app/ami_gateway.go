package app

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"
)

// AMIGateway talks directly to Asterisk Manager Interface. Keeping the adapter
// in Go makes the manager image portable and avoids placing SMS text or AKA
// material in process arguments.
type AMIGateway struct {
	cfg  AMIConfig
	diag *DiagnosticRecorder
}

func (g *AMIGateway) SetDiagnostics(diagnostics *DiagnosticRecorder) {
	g.diag = diagnostics
}

func NewAMIGateway(cfg AMIConfig) (*AMIGateway, error) {
	if strings.TrimSpace(cfg.Address) == "" || strings.TrimSpace(cfg.Username) == "" || cfg.Secret == "" {
		return nil, errors.New("asterisk_ami requires address, username, and secret")
	}
	if !e164Pattern.MatchString(strings.TrimSpace(cfg.FromNumber)) {
		return nil, errors.New("asterisk_ami requires an E.164 from_number")
	}
	if strings.TrimSpace(cfg.Realm) == "" {
		return nil, errors.New("asterisk_ami requires realm")
	}
	return &AMIGateway{cfg: cfg}, nil
}

func (g *AMIGateway) Status(ctx context.Context) (RuntimeStatus, error) {
	response, err := g.action(ctx, map[string]string{
		"Action":  "Command",
		"Command": "pjsip show registrations",
	})
	status := RuntimeStatus{Provider: "asterisk_ami", UpdatedAt: time.Now().UTC()}
	if err != nil {
		status.Detail = err.Error()
		return status, err
	}
	detail := strings.Join(response["Output"], "\n")
	status.Detail = detail
	status.Ready = strings.Contains(detail, "Registered")
	status.SIMReady, status.TunnelReady, status.IMSReady = status.Ready, status.Ready, status.Ready
	if !status.Ready {
		return status, errors.New("IMS is not registered")
	}
	return status, nil
}

func (g *AMIGateway) SendSMS(ctx context.Context, messageID, to, text string) (SMSSubmitResult, error) {
	response, err := g.action(ctx, map[string]string{
		"Action":   "MessageSend",
		"To":       "pjsip:volte_ims/" + to + "@volte_ims",
		"From":     "sip:" + g.cfg.FromNumber + "@" + g.cfg.Realm,
		"Body":     text,
		"Variable": "VITI_MESSAGE_ID=" + messageID,
	})
	if err != nil {
		return SMSSubmitResult{State: "failed"}, err
	}
	if first(response, "Response") != "Success" {
		return SMSSubmitResult{State: "failed"}, fmt.Errorf("Asterisk rejected SMS: %s", first(response, "Message"))
	}
	// AMI Success means only that Asterisk handed the message to its protocol
	// handler. RP-ACK and TP-STATUS-REPORT events provide stronger evidence.
	return SMSSubmitResult{State: "submitted", DeliveryReportExpected: true, Detail: "Submitted to Asterisk; awaiting carrier acknowledgement."}, nil
}

func (g *AMIGateway) StartCall(context.Context, string, string) error {
	return errors.New("interactive calls must be placed through the WebRTC endpoint")
}

func (g *AMIGateway) HangupCall(ctx context.Context, _ string) error {
	response, err := g.action(ctx, map[string]string{
		"Action":  "Command",
		"Command": "channel request hangup all",
	})
	if err != nil {
		return err
	}
	if value := first(response, "Response"); value != "Success" && value != "Follows" {
		return fmt.Errorf("Asterisk rejected hangup: %s", first(response, "Message"))
	}
	return nil
}

func (g *AMIGateway) DropConferenceLeg(ctx context.Context, channel string) error {
	response, err := g.action(ctx, map[string]string{
		"Action": "Hangup", "Channel": channel, "Cause": "16",
	})
	if err != nil {
		return err
	}
	if first(response, "Response") != "Success" {
		message := first(response, "Message")
		if amiTargetAlreadyGone(message) {
			return nil
		}
		return fmt.Errorf("Asterisk rejected directed leg hangup: %s", message)
	}
	return nil
}

func (g *AMIGateway) EndConference(ctx context.Context, conference string) error {
	response, err := g.action(ctx, map[string]string{
		"Action": "ConfbridgeKick", "Conference": conference, "Channel": "all",
	})
	if err != nil {
		return err
	}
	if first(response, "Response") != "Success" {
		message := first(response, "Message")
		if amiTargetAlreadyGone(message) {
			return nil
		}
		return fmt.Errorf("Asterisk rejected conference hangup: %s", message)
	}
	return nil
}

func amiTargetAlreadyGone(message string) bool {
	normalized := strings.ToLower(strings.TrimSpace(message))
	return strings.Contains(normalized, "no such channel") ||
		strings.Contains(normalized, "channel not found") ||
		strings.Contains(normalized, "no such conference") ||
		strings.Contains(normalized, "conference not found") ||
		strings.Contains(normalized, "conference does not exist") ||
		strings.Contains(normalized, "no active conference")
}

func (g *AMIGateway) SetIMSRegistration(ctx context.Context, enabled bool) error {
	command := "pjsip send unregister volte_ims"
	if enabled {
		command = "pjsip send register volte_ims"
	}
	response, err := g.action(ctx, map[string]string{
		"Action":  "Command",
		"Command": command,
	})
	if err != nil {
		return err
	}
	if value := first(response, "Response"); value != "Success" && value != "Follows" {
		return fmt.Errorf("Asterisk rejected IMS registration change: %s", first(response, "Message"))
	}
	return nil
}

func (g *AMIGateway) action(ctx context.Context, fields map[string]string) (map[string][]string, error) {
	started := time.Now()
	actionID := fmt.Sprintf("ami-%d", started.UnixNano())
	g.diag.RecordEvent("manager.ami", "action.started", map[string]any{"action_id": actionID, "address": g.cfg.Address, "fields": fields})
	dialer := net.Dialer{Timeout: 5 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", g.cfg.Address)
	if err != nil {
		g.diag.RecordEvent("manager.ami", "connect.failed", map[string]any{"action_id": actionID, "duration_ns": time.Since(started).Nanoseconds(), "error": err.Error()})
		return nil, fmt.Errorf("connect Asterisk AMI: %w", err)
	}
	g.diag.RecordEvent("manager.ami", "connected", map[string]any{"action_id": actionID, "local_address": conn.LocalAddr().String(), "remote_address": conn.RemoteAddr().String()})
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	reader := bufio.NewReader(conn)
	if _, err := reader.ReadString('\n'); err != nil {
		g.diag.RecordEvent("manager.ami", "banner.failed", map[string]any{"action_id": actionID, "error": err.Error()})
		return nil, fmt.Errorf("read Asterisk AMI banner: %w", err)
	}
	loginFields := map[string]string{
		"Action":   "Login",
		"Username": g.cfg.Username,
		"Secret":   g.cfg.Secret,
		"Events":   "off",
	}
	g.diag.RecordEvent("manager.ami", "login.sent", map[string]any{"action_id": actionID, "fields": loginFields})
	login, err := writeAMIAction(conn, reader, loginFields)
	if err != nil {
		g.diag.RecordEvent("manager.ami", "login.failed", map[string]any{"action_id": actionID, "error": err.Error()})
		return nil, err
	}
	g.diag.RecordEvent("manager.ami", "login.received", map[string]any{"action_id": actionID, "response": login})
	if first(login, "Response") != "Success" {
		return nil, errors.New("Asterisk AMI login failed")
	}
	response, err := writeAMIAction(conn, reader, fields)
	g.diag.RecordEvent("manager.ami", "action.completed", map[string]any{"action_id": actionID, "duration_ns": time.Since(started).Nanoseconds(), "fields": fields, "response": response, "error": errorString(err)})
	if err == nil {
		_, _ = writeAMIAction(conn, reader, map[string]string{"Action": "Logoff"})
	}
	return response, err
}

func writeAMIAction(conn io.Writer, reader *bufio.Reader, fields map[string]string) (map[string][]string, error) {
	for _, key := range []string{"Action", "Username", "Secret", "Events", "Command", "To", "From", "Body", "Variable", "Channel", "Cause", "Conference"} {
		value, ok := fields[key]
		if !ok {
			continue
		}
		if strings.ContainsAny(value, "\r\n") {
			return nil, fmt.Errorf("invalid AMI field %s", key)
		}
		if _, err := fmt.Fprintf(conn, "%s: %s\r\n", key, value); err != nil {
			return nil, err
		}
	}
	if _, err := io.WriteString(conn, "\r\n"); err != nil {
		return nil, err
	}
	return readAMIFrame(reader)
}

func readAMIFrame(reader *bufio.Reader) (map[string][]string, error) {
	frame := map[string][]string{}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if len(frame) != 0 {
				return frame, nil
			}
			continue
		}
		key, value, ok := strings.Cut(line, ": ")
		if ok {
			frame[key] = append(frame[key], value)
		}
	}
}

func first(frame map[string][]string, key string) string {
	if values := frame[key]; len(values) != 0 {
		return values[0]
	}
	return ""
}
