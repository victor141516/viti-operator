package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
)

// CommandAuthenticator adapts any local modem, PC/SC reader, HSM, or vendor
// helper to the same AKA contract used by OpenStick and the Android agent. The
// challenge is sent on stdin so RAND/AUTN never appear in the process list.
type CommandAuthenticator struct {
	Spec CommandSpec
}

func (a CommandAuthenticator) AuthenticateAKA(ctx context.Context, rand16, autn16 []byte) (AKAResult, error) {
	request := struct {
		RAND string `json:"rand"`
		AUTN string `json:"autn"`
	}{
		RAND: base64.RawStdEncoding.EncodeToString(rand16),
		AUTN: base64.RawStdEncoding.EncodeToString(autn16),
	}
	input, err := json.Marshal(request)
	if err != nil {
		return AKAResult{}, err
	}
	output, err := runCommand(ctx, a.Spec, append(input, '\n'))
	if err != nil {
		return AKAResult{}, err
	}
	var response AuthResponse
	if err := json.Unmarshal(output, &response); err != nil {
		return AKAResult{}, fmt.Errorf("SIM authentication command returned invalid JSON: %w", err)
	}
	if response.Error != "" {
		return AKAResult{}, fmt.Errorf("SIM authentication command failed: %s", response.Error)
	}
	return decodeAKAResponse(response)
}
