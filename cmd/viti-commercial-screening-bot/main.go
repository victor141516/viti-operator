package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"viti-operator/internal/realtimebot"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	config, err := realtimebot.LoadScreeningConfig(os.Getenv)
	if err != nil {
		logger.Error("configuration failed", "error", err)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if config.OpenAIAPIKey == "" {
		logger.Warn("commercial screening will fail open because OPENAI_API_KEY is not configured")
	}
	bot := realtimebot.NewCommercialScreeningBot(config, logger)
	if err := bot.Run(ctx); err != nil && ctx.Err() == nil {
		logger.Error("bot stopped", "error", err)
		os.Exit(1)
	}
}
