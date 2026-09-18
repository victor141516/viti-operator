package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	openStickBridgePackage = "lab.codex.simidentity"
	openStickBridge        = openStickBridgePackage + "/.ReadIdentityActivity"
	openStickChallenge     = "/data/local/tmp/aka_challenge.bin"
	openStickResult        = "/data/local/tmp/aka_result"
)

var openStickMaterialPattern = regexp.MustCompile(`material=([A-Za-z0-9+/=]+)`)
var openStickProbeSuccessPattern = regexp.MustCompile(`(?m)^PROBE_EXIT port=[^\r\n ]+ code=(?:0|10)\r?$`)
var openStickProbeFailurePattern = regexp.MustCompile(`(?m)^PROBE_EXIT port=rmnet_sdio0 code=-?[0-9]+\r?$`)

type OpenStickAuthenticator struct {
	ADB       string
	Serial    string
	Timeout   time.Duration
	PollDelay time.Duration
	mu        sync.Mutex
	healthMu  sync.Mutex
	lastCheck time.Time
	lastReady time.Time
	lastError error
	runADB    func(context.Context, ...string) ([]byte, error)
}

func NewOpenStickAuthenticator(adb, serial string) *OpenStickAuthenticator {
	return &OpenStickAuthenticator{ADB: adb, Serial: serial, Timeout: 35 * time.Second, PollDelay: 200 * time.Millisecond}
}

func (a *OpenStickAuthenticator) CheckSIMReady(ctx context.Context) error {
	a.healthMu.Lock()
	defer a.healthMu.Unlock()
	now := time.Now()
	if now.Sub(a.lastCheck) < 5*time.Second {
		return a.lastError
	}
	// AuthenticateAKA deliberately force-stops and restarts the bridge while it
	// owns this mutex. Preserve a recent successful hardware observation during
	// that maintenance window, but never treat an unproven or already failing
	// AKA operation as evidence that the SIM transport is healthy.
	if !a.mu.TryLock() {
		if !a.lastReady.IsZero() && now.Sub(a.lastReady) < 30*time.Second {
			return nil
		}
		if a.lastError != nil {
			return a.lastError
		}
		return errors.New("OpenStick authentication is in progress; SIM readiness has not been confirmed")
	}
	defer a.mu.Unlock()
	rawErr := a.checkSIMReadyNow(ctx)
	err := rawErr
	// ADB on Windows occasionally exceeds the short status-probe deadline even
	// though the USB device never left. Preserve a recent successful observation
	// for transport timeouts only. Deterministic evidence such as ABSENT,
	// offline, or device-not-found still fails immediately.
	if err != nil && isTransientReadinessError(err) && !a.lastReady.IsZero() && now.Sub(a.lastReady) < 30*time.Second {
		err = nil
	}
	a.lastCheck = now
	a.lastError = err
	if rawErr == nil {
		a.lastReady = now
	}
	return err
}

func (a *OpenStickAuthenticator) checkSIMReadyNow(ctx context.Context) error {
	output, err := a.run(ctx, "get-state")
	if err != nil {
		return fmt.Errorf("OpenStick disconnected: %w", err)
	}
	if strings.TrimSpace(string(output)) != "device" {
		return fmt.Errorf("OpenStick unavailable: ADB state is %q", strings.TrimSpace(string(output)))
	}
	output, err = a.run(ctx, "shell", "getprop", "gsm.sim.state")
	if err != nil {
		return fmt.Errorf("OpenStick SIM state unavailable: %w", err)
	}
	if !openStickSIMReady(string(output)) {
		return fmt.Errorf("OpenStick SIM is not ready: state is %q", strings.TrimSpace(string(output)))
	}
	output, err = a.run(ctx, "shell", "pm", "path", openStickBridgePackage)
	if err != nil {
		return fmt.Errorf("OpenStick SIM bridge package check failed: %w", err)
	}
	if !strings.Contains(string(output), "package:") {
		return errors.New("OpenStick SIM bridge package is unavailable")
	}
	if _, err = a.run(ctx, "shell", "test", "-x", "/data/local/tmp/qmi_auth_aka"); err != nil {
		return fmt.Errorf("OpenStick SIM bridge helper check failed: %w", err)
	}
	return nil
}

func isTransientReadinessError(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}

func openStickSIMReady(value string) bool {
	for _, state := range strings.FieldsFunc(strings.ToUpper(value), func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\r' }) {
		if state == "READY" {
			return true
		}
	}
	return false
}

func (a *OpenStickAuthenticator) AuthenticateAKA(ctx context.Context, rand16, autn16 []byte) (AKAResult, error) {
	if len(rand16) != 16 || len(autn16) != 16 {
		return AKAResult{}, errors.New("RAND and AUTN must be 16 bytes")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	challenge := make([]byte, 0, 32)
	challenge = append(challenge, rand16...)
	challenge = append(challenge, autn16...)
	tmp, err := os.CreateTemp("", "viti-aka-*.bin")
	if err != nil {
		return AKAResult{}, err
	}
	path := tmp.Name()
	defer os.Remove(path)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return AKAResult{}, err
	}
	if _, err := tmp.Write(challenge); err != nil {
		tmp.Close()
		return AKAResult{}, err
	}
	if err := tmp.Close(); err != nil {
		return AKAResult{}, err
	}
	for index := range challenge {
		challenge[index] = 0
	}

	defer a.cleanup()
	if _, err := a.run(ctx, "shell", "rm", "-f", openStickChallenge, openStickResult); err != nil {
		return AKAResult{}, err
	}
	if _, err := a.run(ctx, "push", path, openStickChallenge); err != nil {
		return AKAResult{}, err
	}
	if _, err := a.run(ctx, "shell", "chmod", "644", openStickChallenge); err != nil {
		return AKAResult{}, err
	}
	deadline := time.Now().Add(a.timeout())
	probeFailed := false
	for attempt := 0; attempt < 2; attempt++ {
		if _, err := a.run(ctx, "shell", "rm", "-f", openStickResult); err != nil {
			return AKAResult{}, err
		}
		if _, err := a.run(ctx, "shell", "touch", openStickResult); err != nil {
			return AKAResult{}, err
		}
		if _, err := a.run(ctx, "shell", "chmod", "666", openStickResult); err != nil {
			return AKAResult{}, err
		}
		_, _ = a.run(ctx, "shell", "am", "force-stop", openStickBridgePackage)
		if _, err := a.run(ctx, "shell", "am", "start", "-n", openStickBridge); err != nil {
			return AKAResult{}, err
		}
		for time.Now().Before(deadline) {
			probe, probeErr := a.run(ctx, "shell", "cat", openStickResult)
			if probeErr == nil && openStickProbeSuccessPattern.Match(probe) {
				// AkaMaterialProvider deletes its private result after every query,
				// including an empty one.  Querying while qmi_auth_aka still has
				// that file open unlinks the inode and loses an otherwise successful
				// result.  PROBE_EXIT is appended only after waitFor(), so this is the
				// first point at which the destructive provider read is safe.
				output, queryErr := a.run(ctx, "shell", "content", "query", "--uri", "content://lab.codex.simidentity.aka/material")
				if queryErr != nil {
					return AKAResult{}, fmt.Errorf("OpenStick AKA material read failed: %w", queryErr)
				}
				match := openStickMaterialPattern.FindSubmatch(output)
				if len(match) != 2 {
					return AKAResult{}, errors.New("OpenStick AKA helper completed without readable material")
				}
				material, decodeErr := base64.StdEncoding.DecodeString(string(match[1]))
				if decodeErr != nil {
					return AKAResult{}, errors.New("OpenStick returned invalid Base64 material")
				}
				return parseOpenStickMaterial(material)
			}
			if probeErr == nil && openStickProbeFailurePattern.Match(probe) {
				probeFailed = true
				break
			}
			select {
			case <-ctx.Done():
				return AKAResult{}, ctx.Err()
			case <-time.After(a.pollDelay()):
			}
		}
	}
	if probeFailed {
		return AKAResult{}, errors.New("OpenStick AKA helper failed on every modem interface")
	}
	return AKAResult{}, errors.New("OpenStick AKA response timed out")
}

func (a *OpenStickAuthenticator) run(ctx context.Context, args ...string) ([]byte, error) {
	if a.runADB != nil {
		return a.runADB(ctx, args...)
	}
	command := exec.CommandContext(ctx, a.ADB, append([]string{"-s", a.Serial}, args...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return output, fmt.Errorf("ADB %s interrupted: %w: %s", args[0], ctxErr, strings.TrimSpace(string(output)))
		}
		return output, fmt.Errorf("ADB %s failed: %w: %s", args[0], err, strings.TrimSpace(string(output)))
	}
	return output, nil
}

func (a *OpenStickAuthenticator) cleanup() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = a.run(ctx, "shell", "am", "force-stop", openStickBridgePackage)
	_, _ = a.run(ctx, "shell", "rm", "-f", openStickChallenge, openStickResult)
}

func (a *OpenStickAuthenticator) timeout() time.Duration {
	if a.Timeout <= 0 {
		return 35 * time.Second
	}
	return a.Timeout
}

func (a *OpenStickAuthenticator) pollDelay() time.Duration {
	if a.PollDelay <= 0 {
		return 200 * time.Millisecond
	}
	return a.PollDelay
}

func parseOpenStickMaterial(material []byte) (AKAResult, error) {
	if len(material) >= 7 && bytes.Equal(material[:4], []byte("AKA1")) {
		resLength, ckLength, ikLength := int(material[4]), int(material[5]), int(material[6])
		if resLength < 4 || resLength > 16 || ckLength != 16 || ikLength != 16 || len(material) != 7+resLength+ckLength+ikLength {
			return AKAResult{}, errors.New("OpenStick AKA1 material has invalid lengths")
		}
		offset := 7
		return AKAResult{
			RES: append([]byte(nil), material[offset:offset+resLength]...),
			CK:  append([]byte(nil), material[offset+resLength:offset+resLength+ckLength]...),
			IK:  append([]byte(nil), material[offset+resLength+ckLength:]...),
		}, nil
	}
	if len(material) >= 6 && bytes.Equal(material[:4], []byte("AKAR")) {
		resLength, keysLength := int(material[4]), int(material[5])
		if resLength < 4 || resLength > 16 || len(material) != 6+resLength+keysLength {
			return AKAResult{}, errors.New("OpenStick AKAR material has invalid lengths")
		}
		res := append([]byte(nil), material[6:6+resLength]...)
		encodedKeys := material[6+resLength:]
		keys, err := base64.RawStdEncoding.DecodeString(strings.TrimRight(string(encodedKeys), "="))
		if err != nil || len(keys) != 32 {
			return AKAResult{}, errors.New("OpenStick AKAR keys are malformed")
		}
		return AKAResult{RES: res, CK: append([]byte(nil), keys[:16]...), IK: append([]byte(nil), keys[16:]...)}, nil
	}
	return AKAResult{}, errors.New("OpenStick returned an unknown AKA material format")
}
