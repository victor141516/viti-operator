package realtimebot

import (
	"errors"
	"net/url"
	"strings"
)

const defaultInstructions = "You are answering a personal phone call. Speak naturally, briefly, and in the caller's language. Do not claim to be a human."

type Config struct {
	VitiURL      string
	VitiAPIKey   string
	OpenAIAPIKey string
	OpenAIURL    string
	Model        string
	Voice        string
	Instructions string
	ServiceName  string
	JoinMode     string
}

func LoadConfig(getenv func(string) string) (Config, error) {
	config := Config{
		VitiURL:      valueOrDefault(getenv("VITI_URL"), "http://manager:8787"),
		VitiAPIKey:   strings.TrimSpace(getenv("VITI_API_KEY")),
		OpenAIAPIKey: strings.TrimSpace(getenv("OPENAI_API_KEY")),
		OpenAIURL:    valueOrDefault(getenv("OPENAI_REALTIME_URL"), "wss://api.openai.com/v1/realtime"),
		Model:        valueOrDefault(getenv("OPENAI_REALTIME_MODEL"), "gpt-realtime"),
		Voice:        valueOrDefault(getenv("OPENAI_REALTIME_VOICE"), "marin"),
		Instructions: valueOrDefault(getenv("OPENAI_REALTIME_INSTRUCTIONS"), defaultInstructions),
		ServiceName:  valueOrDefault(getenv("VITI_SERVICE_NAME"), "openai-realtime"),
		JoinMode:     valueOrDefault(getenv("VITI_OPENAI_JOIN_MODE"), "all"),
	}
	if config.VitiAPIKey == "" {
		return Config{}, errors.New("VITI_API_KEY is required")
	}
	if _, err := integrationURL(config.VitiURL); err != nil {
		return Config{}, err
	}
	openAIURL, err := url.Parse(config.OpenAIURL)
	if err != nil || (openAIURL.Scheme != "ws" && openAIURL.Scheme != "wss") || openAIURL.Host == "" {
		return Config{}, errors.New("OPENAI_REALTIME_URL must be a ws:// or wss:// URL")
	}
	if config.Model == "" || config.Voice == "" || config.ServiceName == "" {
		return Config{}, errors.New("model, voice, and service name must not be empty")
	}
	if config.JoinMode != "all" && config.JoinMode != "targeted" {
		return Config{}, errors.New("VITI_OPENAI_JOIN_MODE must be all or targeted")
	}
	return config, nil
}

func valueOrDefault(value, fallback string) string {
	if value = strings.TrimSpace(value); value != "" {
		return value
	}
	return fallback
}

func integrationURL(base string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(base))
	if err != nil || parsed.Host == "" {
		return "", errors.New("VITI_URL must be an http://, https://, ws://, or wss:// URL")
	}
	switch parsed.Scheme {
	case "http":
		parsed.Scheme = "ws"
	case "https":
		parsed.Scheme = "wss"
	case "ws", "wss":
	default:
		return "", errors.New("VITI_URL must be an http://, https://, ws://, or wss:// URL")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/api/v1/integrations/ws"
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}
