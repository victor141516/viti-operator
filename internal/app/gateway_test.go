package app

import "testing"

func TestClassifySMSSubmitOutput(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   string
	}{
		{name: "verified delivery", output: "SMS_RESULT=delivered-and-verified", want: "delivered"},
		{name: "carrier accepted", output: "SMS_RESULT=network-accepted", want: "accepted"},
		{name: "unconfirmed timeout", output: "SMS_RESULT=submit-report-timeout", want: "submitted"},
		{name: "operator barred", output: "SMS_RESULT=operator-barred rp_cause=10", want: "failed"},
		{name: "network rejected", output: "SMS_RESULT=network-rejected rp_cause=42", want: "failed"},
		{name: "local failure", output: "SMS_RESULT=local-submit-failed", want: "failed"},
		{name: "generic command", output: "OK", want: "submitted"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := classifySMSSubmitOutput([]byte(test.output)).State; got != test.want {
				t.Fatalf("state=%q, want %q", got, test.want)
			}
		})
	}
}
