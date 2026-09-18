package realtimebot

import (
	"errors"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const defaultScreeningInstructions = `Classify the intent of the person calling a private telephone number. After every completed caller turn, output exactly one label and nothing else:
COMMERCIAL when the caller is selling, marketing, soliciting, offering a contract or service improvement, or represents an energy, telephone, insurance, financial, security, or similar provider for commercial purposes.
NON_COMMERCIAL when the caller clearly has a personal, administrative, delivery, appointment, support, emergency, or other non-sales purpose.
UNCERTAIN when there is not yet enough evidence. A greeting, silence, or a request to speak with someone is not enough evidence. Never infer NON_COMMERCIAL merely because the caller is polite. Continue listening after UNCERTAIN.`

type ScreeningConfig struct {
	VitiURL               string
	VitiAPIKey            string
	OpenAIAPIKey          string
	OpenAIURL             string
	Model                 string
	Instructions          string
	ServiceName           string
	HandlerServiceName    string
	GreetingPath          string
	DecisionTimeout       time.Duration
	MediaTimeout          time.Duration
	ClassificationTimeout time.Duration
	HandlerJoinTimeout    time.Duration
}

func LoadScreeningConfig(getenv func(string) string) (ScreeningConfig, error) {
	config := ScreeningConfig{
		VitiURL:               valueOrDefault(getenv("VITI_URL"), "http://manager:8787"),
		VitiAPIKey:            strings.TrimSpace(getenv("VITI_API_KEY")),
		OpenAIAPIKey:          strings.TrimSpace(getenv("OPENAI_API_KEY")),
		OpenAIURL:             valueOrDefault(getenv("OPENAI_REALTIME_URL"), "wss://api.openai.com/v1/realtime"),
		Model:                 valueOrDefault(getenv("OPENAI_SCREENING_MODEL"), "gpt-realtime-mini"),
		Instructions:          valueOrDefault(getenv("OPENAI_SCREENING_INSTRUCTIONS"), defaultScreeningInstructions),
		ServiceName:           valueOrDefault(getenv("VITI_SCREENING_SERVICE_NAME"), "commercial-screening"),
		HandlerServiceName:    valueOrDefault(getenv("VITI_COMMERCIAL_HANDLER_SERVICE"), "openai-commercial-handler"),
		GreetingPath:          valueOrDefault(getenv("VITI_SCREENING_GREETING_PATH"), "/bot-media/hello.wav"),
		DecisionTimeout:       2 * time.Second,
		MediaTimeout:          15 * time.Second,
		ClassificationTimeout: 20 * time.Second,
		HandlerJoinTimeout:    8 * time.Second,
	}
	var err error
	if config.DecisionTimeout, err = durationMilliseconds(getenv("VITI_SCREENING_DECISION_TIMEOUT_MS"), config.DecisionTimeout); err != nil {
		return ScreeningConfig{}, err
	}
	if config.MediaTimeout, err = durationMilliseconds(getenv("VITI_SCREENING_MEDIA_TIMEOUT_MS"), config.MediaTimeout); err != nil {
		return ScreeningConfig{}, err
	}
	if config.ClassificationTimeout, err = durationMilliseconds(getenv("VITI_SCREENING_CLASSIFICATION_TIMEOUT_MS"), config.ClassificationTimeout); err != nil {
		return ScreeningConfig{}, err
	}
	if config.HandlerJoinTimeout, err = durationMilliseconds(getenv("VITI_SCREENING_HANDLER_TIMEOUT_MS"), config.HandlerJoinTimeout); err != nil {
		return ScreeningConfig{}, err
	}
	if config.VitiAPIKey == "" {
		return ScreeningConfig{}, errors.New("VITI_API_KEY is required")
	}
	if _, err := integrationURL(config.VitiURL); err != nil {
		return ScreeningConfig{}, err
	}
	openAIURL, err := url.Parse(config.OpenAIURL)
	if err != nil || (openAIURL.Scheme != "ws" && openAIURL.Scheme != "wss") || openAIURL.Host == "" {
		return ScreeningConfig{}, errors.New("OPENAI_REALTIME_URL must be a ws:// or wss:// URL")
	}
	if config.Model == "" || config.ServiceName == "" || config.HandlerServiceName == "" || config.GreetingPath == "" {
		return ScreeningConfig{}, errors.New("model, service names, and greeting path must not be empty")
	}
	return config, nil
}

func durationMilliseconds(value string, fallback time.Duration) (time.Duration, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback, nil
	}
	milliseconds, err := strconv.ParseInt(value, 10, 64)
	if err != nil || milliseconds <= 0 {
		return 0, errors.New("screening timeout values must be positive milliseconds")
	}
	return time.Duration(milliseconds) * time.Millisecond, nil
}

func (config ScreeningConfig) classifierConfig() Config {
	return Config{
		OpenAIAPIKey: config.OpenAIAPIKey, OpenAIURL: config.OpenAIURL,
		Model: config.Model, Instructions: config.Instructions,
	}
}
