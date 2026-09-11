package app

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// AMIGateway talks directly to Asterisk Manager Interface. Keeping the adapter
// in Go makes the manager image portable and avoids placing SMS text or AKA
// material in process arguments.
type AMIGateway struct {
	cfg           AMIConfig
	diag          *DiagnosticRecorder
	mediaMu       sync.Mutex
	mediaChannels map[string]string
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
	return &AMIGateway{cfg: cfg, mediaChannels: make(map[string]string)}, nil
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

func (g *AMIGateway) JoinServiceMedia(ctx context.Context, request serviceMediaBridgeRequest) error {
	return g.originateServiceMedia(ctx, request, "viti-service-conference", "s")
}

func (g *AMIGateway) StartServiceCall(ctx context.Context, request serviceMediaBridgeRequest, destination string) error {
	if !e164Pattern.MatchString(destination) {
		return errors.New("service call destination must use E.164")
	}
	return g.originateServiceMedia(ctx, request, "from-local", destination)
}

func (g *AMIGateway) EndServiceCall(ctx context.Context, engineCallID string) error {
	return g.EndConference(ctx, engineCallID)
}

func (g *AMIGateway) originateServiceMedia(ctx context.Context, request serviceMediaBridgeRequest, dialplanContext, extension string) error {
	if request.EngineCallID == "" {
		return errors.New("call does not have an active Asterisk conference")
	}
	if !validCallControlID(request.SessionID) || !validCallControlID(request.EngineCallID) ||
		!integrationNamePattern.MatchString(request.ServiceName) || !validCallControlID(request.ConnectionID) ||
		!validCallControlID(request.RecordingID) ||
		(request.TraceID != "" && !validCallControlID(request.TraceID)) {
		return errors.New("invalid service media bridge identifiers")
	}
	if request.RTPPort < 1 || request.RTPPort > 65535 || strings.TrimSpace(request.RTPHost) == "" {
		return errors.New("invalid service media RTP destination")
	}
	if request.Codec != "opus" && request.Codec != "g722" && request.Codec != "ulaw" && request.Codec != "alaw" {
		return errors.New("invalid service media codec")
	}
	variables := strings.Join([]string{
		"VITI_CALL_ID=" + request.EngineCallID,
		"VITI_TRACE_ID=" + request.TraceID,
		"VITI_CLIENT_ID=" + request.ServiceName,
		"VITI_PARTICIPANT_ID=" + request.ServiceName,
		"VITI_CONNECTION_ID=" + request.ConnectionID,
		"VITI_RECORDING_FILE_ID=" + request.RecordingID,
		"VITI_PARTICIPANT_ROLE=service",
	}, ",")
	originateCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	channel, cleanupRequired, err := g.originateAndAwaitChannel(originateCtx, map[string]string{
		"Action": "Originate", "Channel": "UnicastRTP/" + net.JoinHostPort(request.RTPHost, strconv.Itoa(request.RTPPort)) + "/c(" + request.Codec + ")",
		"Context": dialplanContext, "Exten": extension, "Priority": "1", "Timeout": "5000",
		"Async": "true", "Codecs": request.Codec, "ChannelId": request.SessionID, "Variable": variables,
	})
	if err != nil {
		if cleanupRequired {
			cleanupErr := g.cleanupProvisionalServiceMedia(request.SessionID, request.EngineCallID, dialplanContext == "from-local")
			if cleanupErr != nil {
				return fmt.Errorf("%w; provisional originate cleanup failed: %v", err, cleanupErr)
			}
		}
		return err
	}
	g.mediaMu.Lock()
	g.mediaChannels[request.SessionID] = channel
	g.mediaMu.Unlock()
	return nil
}

func (g *AMIGateway) LeaveServiceMedia(ctx context.Context, sessionID string) error {
	if !validCallControlID(sessionID) {
		return errors.New("invalid service media session ID")
	}
	channel, ok := g.serviceMediaChannel(sessionID)
	if !ok {
		return nil
	}
	response, err := g.action(ctx, map[string]string{
		"Action": "Hangup", "Channel": channel, "Cause": "16",
	})
	if err != nil {
		return err
	}
	if first(response, "Response") != "Success" {
		message := first(response, "Message")
		if !amiTargetAlreadyGone(message) {
			return fmt.Errorf("Asterisk rejected service media hangup: %s", message)
		}
	}
	g.mediaMu.Lock()
	delete(g.mediaChannels, sessionID)
	g.mediaMu.Unlock()
	return nil
}

func (g *AMIGateway) SendServiceDTMF(ctx context.Context, sessionID, digit string, durationMS int) error {
	if !validCallControlID(sessionID) || len(digit) != 1 || !strings.Contains("0123456789*#ABCD", digit) {
		return errors.New("invalid service DTMF request")
	}
	channel, ok := g.serviceMediaChannel(sessionID)
	if !ok {
		return errors.New("service media channel is not active")
	}
	response, err := g.action(ctx, map[string]string{
		"Action": "PlayDTMF", "Channel": channel, "Digit": digit,
		"Duration": strconv.Itoa(durationMS), "Receive": "true",
	})
	if err != nil {
		return err
	}
	if first(response, "Response") != "Success" {
		return fmt.Errorf("Asterisk rejected service DTMF: %s", first(response, "Message"))
	}
	return nil
}

func (g *AMIGateway) serviceMediaChannel(sessionID string) (string, bool) {
	g.mediaMu.Lock()
	defer g.mediaMu.Unlock()
	channel, ok := g.mediaChannels[sessionID]
	return channel, ok
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

func (g *AMIGateway) originateAndAwaitChannel(ctx context.Context, fields map[string]string) (string, bool, error) {
	started := time.Now()
	actionID := fmt.Sprintf("ami-originate-%d", started.UnixNano())
	fields["ActionID"] = actionID
	g.diag.RecordEvent("manager.ami", "originate.started", map[string]any{"action_id": actionID, "address": g.cfg.Address, "fields": fields})
	dialer := net.Dialer{Timeout: 5 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", g.cfg.Address)
	if err != nil {
		return "", false, fmt.Errorf("connect Asterisk AMI: %w", err)
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	reader := bufio.NewReader(conn)
	if _, err := reader.ReadString('\n'); err != nil {
		return "", false, fmt.Errorf("read Asterisk AMI banner: %w", err)
	}
	login, err := writeAMIAction(conn, reader, map[string]string{
		"Action": "Login", "Username": g.cfg.Username, "Secret": g.cfg.Secret, "Events": "on",
	})
	if err != nil {
		return "", false, err
	}
	if first(login, "Response") != "Success" {
		return "", false, errors.New("Asterisk AMI login failed")
	}
	if err := writeAMIFields(conn, fields); err != nil {
		return "", false, err
	}
	response, err := readAMIResponse(reader, actionID)
	if err != nil {
		return "", true, err
	}
	if first(response, "Response") != "Success" {
		return "", false, fmt.Errorf("Asterisk rejected service media bridge: %s", first(response, "Message"))
	}
	for {
		event, err := readAMIFrame(reader)
		if err != nil {
			return "", true, fmt.Errorf("await Asterisk service media channel: %w", err)
		}
		if first(event, "Event") != "OriginateResponse" || first(event, "ActionID") != actionID {
			continue
		}
		if first(event, "Response") != "Success" {
			return "", false, fmt.Errorf("Asterisk failed to originate service media: %s", first(event, "Reason"))
		}
		channel := strings.TrimSpace(first(event, "Channel"))
		if channel == "" {
			return "", true, errors.New("Asterisk did not report the service media channel")
		}
		g.diag.RecordEvent("manager.ami", "originate.completed", map[string]any{
			"action_id": actionID, "duration_ns": time.Since(started).Nanoseconds(), "channel": channel,
		})
		return channel, false, nil
	}
}

func readAMIResponse(reader *bufio.Reader, actionID string) (map[string][]string, error) {
	for {
		frame, err := readAMIFrame(reader)
		if err != nil {
			return nil, err
		}
		if first(frame, "Response") != "" && first(frame, "ActionID") == actionID {
			return frame, nil
		}
	}
}

func (g *AMIGateway) cleanupProvisionalServiceMedia(sessionID, engineCallID string, endCall bool) error {
	deadline := time.Now().Add(7 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		lookupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		channel, found, err := g.findChannelByUniqueID(lookupCtx, sessionID)
		cancel()
		if err != nil {
			lastErr = err
		} else if found {
			hangupCtx, hangupCancel := context.WithTimeout(context.Background(), 5*time.Second)
			response, hangupErr := g.action(hangupCtx, map[string]string{"Action": "Hangup", "Channel": channel, "Cause": "16"})
			hangupCancel()
			if hangupErr != nil {
				lastErr = hangupErr
			} else if first(response, "Response") != "Success" && !amiTargetAlreadyGone(first(response, "Message")) {
				lastErr = fmt.Errorf("Asterisk rejected provisional media hangup: %s", first(response, "Message"))
			} else {
				lastErr = nil
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if endCall {
		endCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		endErr := g.EndConference(endCtx, engineCallID)
		cancel()
		if endErr != nil {
			lastErr = errors.Join(lastErr, endErr)
		}
	}
	if lastErr != nil {
		return lastErr
	}
	return nil
}

func (g *AMIGateway) findChannelByUniqueID(ctx context.Context, uniqueID string) (string, bool, error) {
	actionID := fmt.Sprintf("ami-channels-%d", time.Now().UnixNano())
	dialer := net.Dialer{Timeout: 2 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", g.cfg.Address)
	if err != nil {
		return "", false, err
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	reader := bufio.NewReader(conn)
	if _, err := reader.ReadString('\n'); err != nil {
		return "", false, err
	}
	login, err := writeAMIAction(conn, reader, map[string]string{
		"Action": "Login", "Username": g.cfg.Username, "Secret": g.cfg.Secret, "Events": "off",
	})
	if err != nil || first(login, "Response") != "Success" {
		return "", false, errors.New("Asterisk AMI login failed during provisional cleanup")
	}
	if err := writeAMIFields(conn, map[string]string{"Action": "CoreShowChannels", "ActionID": actionID}); err != nil {
		return "", false, err
	}
	response, err := readAMIResponse(reader, actionID)
	if err != nil {
		return "", false, err
	}
	if first(response, "Response") != "Success" {
		return "", false, fmt.Errorf("Asterisk rejected channel lookup: %s", first(response, "Message"))
	}
	channel := ""
	for {
		frame, err := readAMIFrame(reader)
		if err != nil {
			return "", false, err
		}
		if first(frame, "ActionID") != actionID {
			continue
		}
		switch first(frame, "Event") {
		case "CoreShowChannel":
			if first(frame, "Uniqueid") == uniqueID {
				channel = strings.TrimSpace(first(frame, "Channel"))
			}
		case "CoreShowChannelsComplete":
			return channel, channel != "", nil
		}
	}
}

func writeAMIAction(conn io.Writer, reader *bufio.Reader, fields map[string]string) (map[string][]string, error) {
	if err := writeAMIFields(conn, fields); err != nil {
		return nil, err
	}
	return readAMIFrame(reader)
}

func writeAMIFields(conn io.Writer, fields map[string]string) error {
	for _, key := range []string{"Action", "ActionID", "Username", "Secret", "Events", "Command", "To", "From", "Body", "Variable", "Channel", "Cause", "Conference", "Context", "Exten", "Priority", "Timeout", "Async", "Codecs", "ChannelId", "Digit", "Duration", "Receive"} {
		value, ok := fields[key]
		if !ok {
			continue
		}
		if strings.ContainsAny(value, "\r\n") {
			return fmt.Errorf("invalid AMI field %s", key)
		}
		if _, err := fmt.Fprintf(conn, "%s: %s\r\n", key, value); err != nil {
			return err
		}
	}
	if _, err := io.WriteString(conn, "\r\n"); err != nil {
		return err
	}
	return nil
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
