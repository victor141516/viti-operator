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
	config, err := realtimebot.LoadConfig(os.Getenv)
	if err != nil {
		logger.Error("configuration failed", "error", err)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if config.OpenAIAPIKey == "" {
		logger.Warn("OpenAI Realtime bot is idle because OPENAI_API_KEY is not configured")
		<-ctx.Done()
		return
	}
	bot := realtimebot.New(config, logger)
	if err := bot.Run(ctx); err != nil && ctx.Err() == nil {
		logger.Error("bot stopped", "error", err)
		os.Exit(1)
	}
}
