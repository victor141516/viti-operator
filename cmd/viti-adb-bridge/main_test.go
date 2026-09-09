package main

import "testing"

func TestValidateAddresses(t *testing.T) {
	tests := []struct {
		name       string
		listen     string
		upstream   string
		shouldFail bool
	}{
		{name: "WSL private interface", listen: "172.23.192.1:5038", upstream: "127.0.0.1:5037"},
		{name: "loopback development", listen: "127.0.0.1:5038", upstream: "127.0.0.1:5037"},
		{name: "wildcard listener", listen: "0.0.0.0:5038", upstream: "127.0.0.1:5037", shouldFail: true},
		{name: "public listener", listen: "192.0.2.10:5038", upstream: "127.0.0.1:5037", shouldFail: true},
		{name: "remote upstream", listen: "172.23.192.1:5038", upstream: "192.168.1.10:5037", shouldFail: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateAddresses(test.listen, test.upstream)
			if (err != nil) != test.shouldFail {
				t.Fatalf("validateAddresses() error = %v, shouldFail = %v", err, test.shouldFail)
			}
		})
	}
}
