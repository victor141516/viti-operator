package app

import (
	"context"
	"encoding/base64"
	"os"
	"testing"
)

func TestCommandAuthenticatorRoundTrip(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") == "1" {
		_, _ = os.Stdout.WriteString(`{"res":"AQIDBA","ck":"AAECAwQFBgcICQoLDA0ODw","ik":"EBESExQVFhcYGRobHB0eHw"}`)
		os.Exit(0)
	}
	auth := CommandAuthenticator{Spec: CommandSpec{
		Executable: os.Args[0],
		Args:       []string{"-test.run=TestCommandAuthenticatorRoundTrip"},
		Env:        map[string]string{"GO_WANT_HELPER_PROCESS": "1"},
	}}
	result, err := auth.AuthenticateAKA(context.Background(), make([]byte, 16), make([]byte, 16))
	if err != nil {
		t.Fatal(err)
	}
	if got := base64.RawStdEncoding.EncodeToString(result.RES); got != "AQIDBA" {
		t.Fatalf("unexpected RES: %s", got)
	}
	if len(result.CK) != 16 || len(result.IK) != 16 {
		t.Fatalf("unexpected key lengths CK=%d IK=%d", len(result.CK), len(result.IK))
	}
}

func TestCommandInputExpansion(t *testing.T) {
	spec := expandCommand(CommandSpec{Stdin: "{{text}}"}, map[string]string{"text": "hello"})
	if got := string(commandInput(spec)); got != "hello" {
		t.Fatalf("unexpected stdin: %q", got)
	}
}
