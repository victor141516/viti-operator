package realtimebot

import "testing"

func TestLoadConfigDefaultsAndBuildsIntegrationURL(t *testing.T) {
	values := map[string]string{"VITI_API_KEY": "test-key"}
	config, err := LoadConfig(func(key string) string { return values[key] })
	if err != nil {
		t.Fatal(err)
	}
	if config.Model != "gpt-realtime" || config.Voice != "marin" || config.ServiceName != "openai-realtime" || config.JoinMode != "all" {
		t.Fatalf("unexpected defaults: %#v", config)
	}
	endpoint, err := integrationURL("https://viti.example/base/")
	if err != nil {
		t.Fatal(err)
	}
	if endpoint != "wss://viti.example/base/api/v1/integrations/ws" {
		t.Fatalf("integration URL = %q", endpoint)
	}
}

func TestLoadConfigAcceptsTargetedJoinMode(t *testing.T) {
	config, err := LoadConfig(func(key string) string {
		values := map[string]string{"VITI_API_KEY": "test-key", "VITI_OPENAI_JOIN_MODE": "targeted"}
		return values[key]
	})
	if err != nil {
		t.Fatal(err)
	}
	if config.JoinMode != "targeted" {
		t.Fatalf("join mode = %q", config.JoinMode)
	}
}

func TestLoadConfigAllowsMissingOpenAIKeyForIdleDeployment(t *testing.T) {
	config, err := LoadConfig(func(key string) string {
		if key == "VITI_API_KEY" {
			return "test-key"
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	if config.OpenAIAPIKey != "" {
		t.Fatal("missing OpenAI key was not preserved")
	}
}
