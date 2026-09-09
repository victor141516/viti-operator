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
	requests := make(chan map[string][]string, 16)
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
	result, err := gateway.SendSMS(ctx, "+34987654321", "hello")
	if err != nil {
		t.Fatal(err)
	}
	if result.State != "accepted" {
		t.Fatalf("SMS state=%q, want accepted", result.State)
	}
	if err := gateway.SetIMSRegistration(ctx, false); err != nil {
		t.Fatal(err)
	}
	if err := gateway.SetIMSRegistration(ctx, true); err != nil {
		t.Fatal(err)
	}
	var sawMessage bool
	var sawUnregister bool
	var sawRegister bool
	for len(requests) > 0 {
		request := <-requests
		if first(request, "Action") == "MessageSend" {
			sawMessage = first(request, "Body") == "hello" && strings.Contains(first(request, "To"), "+34987654321")
		}
		if first(request, "Action") == "Command" {
			switch first(request, "Command") {
			case "pjsip send unregister volte_ims":
				sawUnregister = true
			case "pjsip send register volte_ims":
				sawRegister = true
			}
		}
	}
	if !sawMessage {
		t.Fatal("fake AMI did not receive MessageSend")
	}
	if !sawUnregister || !sawRegister {
		t.Fatalf("IMS commands missing: unregister=%t register=%t", sawUnregister, sawRegister)
	}
}

func serveFakeAMI(conn net.Conn, requests chan<- map[string][]string) {
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
		case "Command":
			_, _ = fmt.Fprint(conn, "Response: Follows\r\nOutput: volte_ims Registered\r\n\r\n")
		case "Logoff":
			_, _ = fmt.Fprint(conn, "Response: Goodbye\r\n\r\n")
			return
		default:
			_, _ = fmt.Fprint(conn, "Response: Success\r\n\r\n")
		}
	}
}
