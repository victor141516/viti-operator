package realtimebot

import (
	"context"
	"errors"
	"strings"
)

type commercialClassification string

const (
	classificationCommercial    commercialClassification = "commercial"
	classificationNonCommercial commercialClassification = "non_commercial"
	classificationUncertain     commercialClassification = "uncertain"
)

func connectClassifier(ctx context.Context, config Config) (*realtimeSession, error) {
	sessionConfig := map[string]any{
		"type": "realtime", "model": config.Model, "output_modalities": []string{"text"},
		"instructions": config.Instructions,
		"audio": map[string]any{
			"input": map[string]any{
				"format": map[string]any{"type": "audio/pcmu"},
				"turn_detection": map[string]any{
					"type": "server_vad", "create_response": true, "interrupt_response": true,
				},
			},
		},
	}
	return connectRealtimeSession(ctx, config, sessionConfig)
}

func (session *realtimeSession) classify(ctx context.Context) (commercialClassification, error) {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer session.close()
	inputErrors := make(chan error, 1)
	go func() { inputErrors <- session.inputLoop(runCtx) }()
	responses := make(chan commercialClassification, 1)
	readErrors := make(chan error, 1)
	go func() {
		classification, err := session.classificationReadLoop()
		if err != nil {
			readErrors <- err
			return
		}
		responses <- classification
	}()
	select {
	case classification := <-responses:
		return classification, nil
	case err := <-readErrors:
		return classificationUncertain, err
	case err := <-inputErrors:
		return classificationUncertain, err
	case <-ctx.Done():
		return classificationUncertain, ctx.Err()
	}
}

func (session *realtimeSession) classificationReadLoop() (commercialClassification, error) {
	var response strings.Builder
	for {
		var event realtimeEvent
		if err := session.conn.ReadJSON(&event); err != nil {
			return classificationUncertain, err
		}
		switch event.Type {
		case "response.output_text.delta", "response.text.delta":
			response.WriteString(event.Delta)
		case "response.output_text.done", "response.text.done":
			if response.Len() == 0 {
				response.WriteString(event.Text)
			}
		case "response.done":
			classification := parseCommercialClassification(response.String())
			response.Reset()
			if classification != classificationUncertain {
				return classification, nil
			}
		case "error":
			return classificationUncertain, realtimeEventError(event)
		}
	}
}

func parseCommercialClassification(value string) commercialClassification {
	normalized := strings.ToUpper(strings.TrimSpace(value))
	normalized = strings.NewReplacer("-", "_", " ", "_", "\n", "_").Replace(normalized)
	if strings.Contains(normalized, "NON_COMMERCIAL") || strings.Contains(normalized, "NO_COMERCIAL") {
		return classificationNonCommercial
	}
	if strings.Contains(normalized, "COMMERCIAL") || strings.Contains(normalized, "COMERCIAL") {
		return classificationCommercial
	}
	if strings.Contains(normalized, "UNCERTAIN") || strings.Contains(normalized, "INCIERTO") {
		return classificationUncertain
	}
	return classificationUncertain
}

var errClassificationUnavailable = errors.New("commercial classification was unavailable")
