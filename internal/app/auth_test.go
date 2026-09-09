package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"testing"
	"time"
)

func TestAuthBrokerRoundTrip(t *testing.T) {
	broker := NewAuthBroker()
	resultCh := make(chan AKAResult, 1)
	errCh := make(chan error, 1)
	go func() {
		result, err := broker.Submit(context.Background(), "phone", bytes.Repeat([]byte{1}, 16), bytes.Repeat([]byte{2}, 16), time.Second)
		resultCh <- result
		errCh <- err
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	challenge := broker.Poll(ctx, "phone")
	if challenge == nil {
		t.Fatal("expected challenge")
	}
	response := AuthResponse{
		RES: base64.RawStdEncoding.EncodeToString(bytes.Repeat([]byte{3}, 8)),
		CK:  base64.RawStdEncoding.EncodeToString(bytes.Repeat([]byte{4}, 16)),
		IK:  base64.RawStdEncoding.EncodeToString(bytes.Repeat([]byte{5}, 16)),
	}
	if err := broker.Respond("phone", challenge.ID, response); err != nil {
		t.Fatal(err)
	}
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
	result := <-resultCh
	if len(result.RES) != 8 || len(result.CK) != 16 || len(result.IK) != 16 {
		t.Fatalf("unexpected result lengths: %d/%d/%d", len(result.RES), len(result.CK), len(result.IK))
	}
	if err := broker.Respond("phone", challenge.ID, response); err == nil {
		t.Fatal("expected replay response to fail")
	}
}

func TestParseOpenStickAKA1(t *testing.T) {
	material := append([]byte("AKA1"), 8, 16, 16)
	material = append(material, bytes.Repeat([]byte{1}, 8)...)
	material = append(material, bytes.Repeat([]byte{2}, 16)...)
	material = append(material, bytes.Repeat([]byte{3}, 16)...)
	result, err := parseOpenStickMaterial(material)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.RES) != 8 || len(result.CK) != 16 || len(result.IK) != 16 {
		t.Fatalf("unexpected result lengths: %d/%d/%d", len(result.RES), len(result.CK), len(result.IK))
	}
}

func TestParseOpenStickAKAR(t *testing.T) {
	keys := bytes.Repeat([]byte{7}, 32)
	encoded := []byte(base64.RawStdEncoding.EncodeToString(keys))
	material := append([]byte("AKAR"), 8, byte(len(encoded)))
	material = append(material, bytes.Repeat([]byte{1}, 8)...)
	material = append(material, encoded...)
	result, err := parseOpenStickMaterial(material)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.RES) != 8 || !bytes.Equal(result.CK, keys[:16]) || !bytes.Equal(result.IK, keys[16:]) {
		t.Fatal("unexpected parsed result")
	}
}
