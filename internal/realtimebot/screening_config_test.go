package realtimebot

import (
	"testing"
	"time"
)

func TestLoadScreeningConfigDefaults(t *testing.T) {
	config, err := LoadScreeningConfig(func(key string) string {
		if key == "VITI_API_KEY" {
			return "key"
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	if config.ServiceName != "commercial-screening" || config.HandlerServiceName != "openai-commercial-handler" || config.Model != "gpt-realtime-mini" {
		t.Fatalf("screening defaults = %#v", config)
	}
	if config.ClassificationTimeout != 20*time.Second || config.HandlerJoinTimeout != 8*time.Second {
		t.Fatalf("screening timeouts = %#v", config)
	}
}

func TestLoadScreeningConfigRejectsInvalidTimeout(t *testing.T) {
	_, err := LoadScreeningConfig(func(key string) string {
		values := map[string]string{"VITI_API_KEY": "key", "VITI_SCREENING_CLASSIFICATION_TIMEOUT_MS": "0"}
		return values[key]
	})
	if err == nil {
		t.Fatal("zero classification timeout was accepted")
	}
}
