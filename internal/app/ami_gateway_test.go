package app

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

func TestAMIGatewayStatusAndSMS(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	requests := make(chan map[string][]string, 40)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go serveFakeAMI(conn, requests)
		}
	}()

	gateway, err := NewAMIGateway(AMIConfig{
		Address: listener.Addr().String(), Username: "manager", Secret: "secret",
		FromNumber: "+34123456789", Realm: "ims.example.test",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	status, err := gateway.Status(ctx)
	if err != nil || !status.Ready {
		t.Fatalf("status=%+v err=%v", status, err)
	}
	result, err := gateway.SendSMS(ctx, "sms-test-correlation", "+34987654321", "hello")
	if err != nil {
		t.Fatal(err)
	}
	if result.State != "submitted" || !result.DeliveryReportExpected {
		t.Fatalf("SMS result=%+v, want submitted with a delivery report requested", result)
	}
	if err := gateway.SetIMSRegistration(ctx, false); err != nil {
		t.Fatal(err)
	}
	if err := gateway.SetIMSRegistration(ctx, true); err != nil {
		t.Fatal(err)
	}
	if err := gateway.DropConferenceLeg(ctx, "PJSIP/webrtc-0000002a"); err != nil {
		t.Fatal(err)
	}
	if err := gateway.EndConference(ctx, "asterisk-call-42"); err != nil {
		t.Fatal(err)
	}
	if err := gateway.RingClients(ctx, Call{EngineCallID: "asterisk-call-42", TraceID: "trace-42", Peer: "+34987654321"}); err != nil {
		t.Fatal(err)
	}
	if err := gateway.TestIncomingCall(ctx); err != nil {
		t.Fatal(err)
	}
	if err := gateway.NotifyCarrierOutage(ctx); err != nil {
		t.Fatal(err)
	}
	mediaRequest := serviceMediaBridgeRequest{
		SessionID: "media-42", EngineCallID: "asterisk-call-42", TraceID: "trace-42",
		ServiceName: "transcriber", ConnectionID: "integration-42", RecordingID: "track-42", RTPHost: "manager", RTPPort: 32000, Codec: "opus",
	}
	if err := gateway.JoinServiceMedia(ctx, mediaRequest); err != nil {
		t.Fatal(err)
	}
	callRequest := mediaRequest
	callRequest.SessionID = "media-call-43"
	if err := gateway.StartServiceCall(ctx, callRequest, "+34987654321"); err != nil {
		t.Fatal(err)
	}
	if err := gateway.SendServiceDTMF(ctx, "media-42", "#", 300); err != nil {
		t.Fatal(err)
	}
	if err := gateway.LeaveServiceMedia(ctx, "media-42"); err != nil {
		t.Fatal(err)
	}
	var sawMessage bool
	var sawUnregister bool
	var sawRegister bool
	var sawDirectedHangup bool
	var sawConferenceEnd bool
	var sawScreenedRing bool
	var sawIncomingCallTest bool
	var sawCarrierOutageAlert bool
	var sawMediaJoin bool
	var sawServiceCall bool
	var sawMediaDTMF bool
	var sawMediaLeave bool
	for range 36 {
		request := <-requests
		if first(request, "Action") == "MessageSend" {
			sawMessage = first(request, "Body") == "hello" &&
				strings.Contains(first(request, "To"), "+34987654321") &&
				first(request, "Variable") == "VITI_MESSAGE_ID=sms-test-correlation"
		}
		if first(request, "Action") == "Command" {
			switch first(request, "Command") {
			case "pjsip send unregister volte_ims":
				sawUnregister = true
			case "pjsip send register volte_ims":
				sawRegister = true
			}
		}
		if first(request, "Action") == "Hangup" && first(request, "Channel") == "PJSIP/webrtc-0000002a" {
			sawDirectedHangup = true
		}
		if first(request, "Action") == "ConfbridgeKick" {
			sawConferenceEnd = first(request, "Conference") == "asterisk-call-42" && first(request, "Channel") == "all"
		}
		if first(request, "Action") == "Originate" {
			if first(request, "Channel") == "Local/s@viti-ring-clients" {
				sawScreenedRing = first(request, "Application") == "Wait" && first(request, "Data") == "65" &&
					strings.Contains(first(request, "Variable"), "__VITI_CALL_ID=asterisk-call-42")
			}
			if first(request, "Channel") == "Local/inbound@viti-diagnostic" {
				sawIncomingCallTest = first(request, "Application") == "Wait" && first(request, "Data") == "10" &&
					first(request, "Timeout") == "15000" && first(request, "Async") == "true"
			}
			if first(request, "Channel") == "Local/down@viti-service-alert" {
				sawCarrierOutageAlert = first(request, "Application") == "Wait" && first(request, "Data") == "45" &&
					first(request, "Timeout") == "50000" && first(request, "Async") == "true"
			}
			if first(request, "Context") == "viti-service-conference" {
				sawMediaJoin = first(request, "Channel") == "UnicastRTP/manager:32000/c(opus)" &&
					first(request, "ChannelId") == "media-42" && strings.Contains(first(request, "Variable"), "VITI_PARTICIPANT_ROLE=service") &&
					strings.Contains(first(request, "Variable"), "VITI_PARTICIPANT_ID=transcriber") &&
					strings.Contains(first(request, "Variable"), "VITI_RECORDING_FILE_ID=track-42")
			}
			if first(request, "Context") == "from-local" {
				sawServiceCall = first(request, "Exten") == "+34987654321" && first(request, "ChannelId") == "media-call-43"
			}
		}
		if first(request, "Action") == "PlayDTMF" {
			sawMediaDTMF = first(request, "Channel") == "UnicastRTP/manager:32000-00000001" && first(request, "Digit") == "#" &&
				first(request, "Duration") == "300" && first(request, "Receive") == "true"
		}
		if first(request, "Action") == "Hangup" && first(request, "Channel") == "UnicastRTP/manager:32000-00000001" {
			sawMediaLeave = true
		}
	}
	if !sawMessage {
		t.Fatal("fake AMI did not receive MessageSend")
	}
	if !sawUnregister || !sawRegister {
		t.Fatalf("IMS commands missing: unregister=%t register=%t", sawUnregister, sawRegister)
	}
	if !sawDirectedHangup || !sawConferenceEnd || !sawScreenedRing || !sawIncomingCallTest || !sawCarrierOutageAlert {
		t.Fatalf("conference controls missing: directed=%t end=%t screened_ring=%t incoming_test=%t outage_alert=%t", sawDirectedHangup, sawConferenceEnd, sawScreenedRing, sawIncomingCallTest, sawCarrierOutageAlert)
	}
	if !sawMediaJoin || !sawServiceCall || !sawMediaDTMF || !sawMediaLeave {
		t.Fatalf("service media actions missing: join=%t call=%t dtmf=%t leave=%t", sawMediaJoin, sawServiceCall, sawMediaDTMF, sawMediaLeave)
	}
}

func TestAMIGatewaySendsThePreviewedRawRPDUAndOverrides(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	requests := make(chan map[string][]string, 8)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr == nil {
			serveFakeAMI(conn, requests)
		}
	}()
	gateway, err := NewAMIGateway(AMIConfig{Address: listener.Addr().String(), Username: "manager", Secret: "secret", FromNumber: "+34111222333", Realm: "ims.example.test"})
	if err != nil {
		t.Fatal(err)
	}
	preview, err := buildSMSDebugPreview(SMSDebugRequest{To: "+34987654321", Text: "hello"}, SMSDebugDefaults{FromNumber: "+34111222333", Realm: "ims.example.test", SMSC: "+34123456789", AccessNetworkInfo: "IEEE-802.11;country=ES"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := gateway.SendDebugSMS(ctx, "sms-debug", preview); err != nil {
		t.Fatal(err)
	}
	for {
		select {
		case request := <-requests:
			if first(request, "Action") != "MessageSend" {
				continue
			}
			variables := first(request, "Variable")
			for _, required := range []string{"VITI_MESSAGE_ID=sms-debug", "VITI_SMS_DEBUG_RAW=", "VITI_SMS_DEBUG_REQUEST_URI=", "VITI_SMS_DEBUG_FROM_URI=", "VITI_SMS_DEBUG_PANI="} {
				if !strings.Contains(variables, required) {
					t.Fatalf("AMI variables %q do not contain %q", variables, required)
				}
			}
			return
		case <-ctx.Done():
			t.Fatal("fake AMI did not receive debug MessageSend")
		}
	}
}

func TestAMITargetAlreadyGoneOnlyAcceptsMissingResources(t *testing.T) {
	for _, message := range []string{
		"No such channel",
		"Channel not found",
		"No such conference",
		"Conference not found",
		"Conference does not exist",
		"No active conferences.",
	} {
		if !amiTargetAlreadyGone(message) {
			t.Fatalf("missing-resource response %q was not idempotent", message)
		}
	}
	for _, message := range []string{"Permission denied", "Invalid action", "Authentication failed", ""} {
		if amiTargetAlreadyGone(message) {
			t.Fatalf("material AMI failure %q was hidden", message)
		}
	}
}

func TestAMIGatewayCleansAcceptedOriginateWhenResponseEventIsLost(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	requests := make(chan map[string][]string, 16)
	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			go serveUncertainOriginateAMI(conn, requests)
		}
	}()
	gateway, err := NewAMIGateway(AMIConfig{
		Address: listener.Addr().String(), Username: "manager", Secret: "secret",
		FromNumber: "+34123456789", Realm: "ims.example.test",
	})
	if err != nil {
		t.Fatal(err)
	}
	request := serviceMediaBridgeRequest{
		SessionID: "media-uncertain", EngineCallID: "call-uncertain", TraceID: "trace-uncertain",
		ServiceName: "screening-bot", ConnectionID: "connection-uncertain", RecordingID: "track-uncertain",
		RTPHost: "manager", RTPPort: 32001, Codec: "opus",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := gateway.JoinServiceMedia(ctx, request); err == nil {
		t.Fatal("originate succeeded despite losing its correlated response event")
	}
	foundHangup := false
	for len(requests) != 0 {
		action := <-requests
		if first(action, "Action") == "Hangup" && first(action, "Channel") == "UnicastRTP/manager:32001-00000077" {
			foundHangup = true
		}
	}
	if !foundHangup {
		t.Fatal("accepted provisional Originate was not resolved and hung up")
	}
}

func serveUncertainOriginateAMI(conn net.Conn, requests chan<- map[string][]string) {
	defer conn.Close()
	_, _ = fmt.Fprint(conn, "Asterisk Call Manager/5.0\r\n")
	reader := bufio.NewReader(conn)
	for {
		frame, err := readAMIFrame(reader)
		if err != nil {
			return
		}
		requests <- frame
		switch first(frame, "Action") {
		case "Originate":
			actionID := first(frame, "ActionID")
			_, _ = fmt.Fprintf(conn,
				"Event: Newchannel\r\nChannel: unrelated\r\n\r\n"+
					"Response: Success\r\nActionID: %s\r\nMessage: Originate successfully queued\r\n\r\n",
				actionID,
			)
			return
		case "CoreShowChannels":
			actionID := first(frame, "ActionID")
			_, _ = fmt.Fprintf(conn,
				"Response: Success\r\nActionID: %s\r\nEventList: start\r\n\r\n"+
					"Event: CoreShowChannel\r\nActionID: %s\r\nChannel: UnicastRTP/manager:32001-00000077\r\nUniqueid: media-uncertain\r\n\r\n"+
					"Event: CoreShowChannelsComplete\r\nActionID: %s\r\nEventList: Complete\r\n\r\n",
				actionID, actionID, actionID,
			)
		case "Logoff":
			_, _ = fmt.Fprint(conn, "Response: Goodbye\r\n\r\n")
			return
		default:
			_, _ = fmt.Fprint(conn, "Response: Success\r\n\r\n")
		}
	}
}

func serveFakeAMI(conn net.Conn, requests chan<- map[string][]string) {
	defer conn.Close()
	_, _ = fmt.Fprint(conn, "Asterisk Call Manager/5.0\r\n")
	reader := bufio.NewReader(conn)
	originateSequence := 0
	for {
		frame, err := readAMIFrame(reader)
		if err != nil {
			return
		}
		requests <- frame
		switch first(frame, "Action") {
		case "Command":
			_, _ = fmt.Fprint(conn, "Response: Follows\r\nOutput: volte_ims Registered\r\n\r\n")
		case "Originate":
			originateSequence++
			channel := fmt.Sprintf("UnicastRTP/manager:32000-%08d", originateSequence)
			_, _ = fmt.Fprintf(conn,
				"Response: Success\r\nActionID: %s\r\nMessage: Originate successfully queued\r\n\r\n"+
					"Event: OriginateResponse\r\nActionID: %s\r\nResponse: Success\r\nChannel: %s\r\nUniqueid: %s\r\nReason: 4\r\n\r\n",
				first(frame, "ActionID"), first(frame, "ActionID"), channel, first(frame, "ChannelId"),
			)
		case "Logoff":
			_, _ = fmt.Fprint(conn, "Response: Goodbye\r\n\r\n")
			return
		default:
			_, _ = fmt.Fprint(conn, "Response: Success\r\n\r\n")
		}
	}
}
