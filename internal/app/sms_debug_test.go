package app

import (
	"strings"
	"testing"
)

func debugIntPointer(value int) *int { return &value }

func TestBuildSMSDebugPreviewMatchesAsteriskGSM7Envelope(t *testing.T) {
	preview, err := buildSMSDebugPreview(SMSDebugRequest{
		To: "+34987654321", Text: "hello", RPReference: debugIntPointer(1), TPReference: debugIntPointer(0),
	}, SMSDebugDefaults{FromNumber: "+34111222333", Realm: "ims.example.test", SMSC: "+34123456789"})
	if err != nil {
		t.Fatal(err)
	}
	if preview.Encoding != "gsm7" || preview.Multipart || len(preview.Parts) != 1 {
		t.Fatalf("unexpected preview: %+v", preview)
	}
	const want = "00010007914321436587F91331000B914389674523F10000FF05E8329BFD06"
	if got := preview.Parts[0].RPDUHex; got != want {
		t.Fatalf("RPDU = %s, want %s", got, want)
	}
	if preview.Request.DCS == nil || *preview.Request.DCS != 0 || preview.Request.RequestURI != "sip:+34123456789@ims.example.test" {
		t.Fatalf("normalized request = %+v", preview.Request)
	}
}

func TestBuildSMSDebugPreviewProducesEditableMultipartRPDUs(t *testing.T) {
	preview, err := buildSMSDebugPreview(SMSDebugRequest{To: "+34987654321", Text: strings.Repeat("a", 161)}, SMSDebugDefaults{FromNumber: "+34111222333", Realm: "ims.example.test", SMSC: "+34123456789"})
	if err != nil {
		t.Fatal(err)
	}
	if !preview.Multipart || len(preview.Parts) != 2 {
		t.Fatalf("parts = %+v", preview.Parts)
	}
	for index, part := range preview.Parts {
		if part.Part != index+1 || part.Parts != 2 || part.RPReference != 1+index || part.TPReference != index {
			t.Fatalf("part %d metadata = %+v", index, part)
		}
		data, decodeErr := decodeDebugHex(part.RPDUHex)
		if decodeErr != nil || int(data[3])+5 >= len(data) || data[int(data[3])+5]&0x40 == 0 {
			t.Fatalf("part %d does not carry TP-UDHI: %x, err=%v", index, data, decodeErr)
		}
	}
}

func TestBuildSMSDebugPreviewValidatesRawRPDUs(t *testing.T) {
	defaults := SMSDebugDefaults{FromNumber: "+34111222333", Realm: "ims.example.test", SMSC: "+34123456789"}
	preview, err := buildSMSDebugPreview(SMSDebugRequest{To: "+34987654321", Text: "debug", Mode: "raw", RawRPDUs: []string{"00 2A 00 00"}}, defaults)
	if err != nil {
		t.Fatal(err)
	}
	if preview.Parts[0].RPReference != 42 || preview.Parts[0].RPDUHex != "002A0000" {
		t.Fatalf("raw preview = %+v", preview.Parts[0])
	}
	if _, err := buildSMSDebugPreview(SMSDebugRequest{To: "+34987654321", Text: "debug", Mode: "raw", RawRPDUs: []string{"not hex"}}, defaults); err == nil {
		t.Fatal("invalid hexadecimal RPDU was accepted")
	}
}
