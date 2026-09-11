package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfigContainerOverrides(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	data := []byte(`{
  "listen":"127.0.0.1:1",
  "data_file":"/data/state.json",
  "telephony":{"type":"asterisk_ami","ami":{"address":"old","username":"vowifi","secret":"old","from_number":"+34123456789","realm":"old"}},
  "sim_auth":{"type":"disabled"},
  "voice":{"enabled":true,"username":"viti","password":"old"}
}`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VITI_LISTEN", "0.0.0.0:8787")
	t.Setenv("VITI_AMI_ADDRESS", "engine:5038")
	t.Setenv("VITI_AMI_SECRET", "new-ami")
	t.Setenv("VITI_VOICE_PASSWORD", "new-voice")
	t.Setenv("VITI_VOICE_ICE_URLS", " stun:100.64.0.1:3478, stun:100.64.0.2:3478 ")
	t.Setenv("VITI_VOICE_ICE_USERNAME", "test-user")
	t.Setenv("VITI_VOICE_ICE_CREDENTIAL", "test-credential")
	t.Setenv("VITI_SERVICE_MEDIA_ENABLED", "true")
	t.Setenv("VITI_SERVICE_MEDIA_LISTEN", "0.0.0.0:8790")
	t.Setenv("VITI_SERVICE_MEDIA_ADVERTISE", "100.64.0.10")
	t.Setenv("VITI_SERVICE_MEDIA_ASTERISK_HOST", "manager")
	t.Setenv("VITI_SIM_AUTH_TYPE", "remote")
	t.Setenv("VITI_DIAGNOSTICS_DIR", filepath.Join(t.TempDir(), "diagnostics"))
	t.Setenv("VITI_DIAGNOSTICS_ENABLED", "0")
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(cfg)
	if cfg.Listen != "0.0.0.0:8787" || cfg.Telephony.AMI.Address != "engine:5038" ||
		cfg.Telephony.AMI.Secret != "new-ami" || cfg.Voice.Password != "new-voice" ||
		cfg.SIMAuth.Type != "remote" {
		t.Fatalf("environment overrides not applied: %s", encoded)
	}
	if len(cfg.Voice.ICEServers) != 1 || len(cfg.Voice.ICEServers[0].URLs) != 2 ||
		cfg.Voice.ICEServers[0].URLs[0] != "stun:100.64.0.1:3478" ||
		cfg.Voice.ICEServers[0].Username != "test-user" ||
		cfg.Voice.ICEServers[0].Credential != "test-credential" {
		t.Fatalf("ICE URL environment override not applied: %s", encoded)
	}
	if cfg.Diagnostics.Enabled {
		t.Fatal("a diagnostics directory alone must not enable unredacted tracing")
	}
	if !cfg.Voice.ServiceMedia.Enabled || cfg.Voice.ServiceMedia.ListenAddress != "0.0.0.0:8790" ||
		cfg.Voice.ServiceMedia.AdvertiseAddress != "100.64.0.10" || cfg.Voice.ServiceMedia.AsteriskRTPHost != "manager" {
		t.Fatalf("service media environment override not applied: %s", encoded)
	}
}

func TestLoadConfigRequiresExplicitDiagnosticsFlag(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"telephony":{"type":"memory"},"sim_auth":{"type":"disabled"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VITI_DIAGNOSTICS_DIR", filepath.Join(t.TempDir(), "diagnostics"))
	t.Setenv("VITI_DIAGNOSTICS_ENABLED", "true")
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Diagnostics.Enabled {
		t.Fatal("the explicit diagnostics flag should enable unredacted tracing")
	}
}

func TestLoadConfigNormalizesScreeningPipeline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"telephony":{"type":"memory"},"sim_auth":{"type":"disabled"},"screening_pipeline":[" spam-filter ","voice-check"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.ScreeningPipeline) != 2 || cfg.ScreeningPipeline[0] != "spam-filter" || cfg.ScreeningPipeline[1] != "voice-check" {
		t.Fatalf("screening pipeline = %#v", cfg.ScreeningPipeline)
	}
}

func TestLoadConfigRejectsDuplicateScreeningServices(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"telephony":{"type":"memory"},"sim_auth":{"type":"disabled"},"screening_pipeline":["spam-filter","spam-filter"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "duplicate service") {
		t.Fatalf("duplicate pipeline error = %v", err)
	}
}
