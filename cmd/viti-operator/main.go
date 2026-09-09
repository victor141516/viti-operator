package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"viti-operator/internal/app"
)

func main() {
	configPath := flag.String("config", "", "path to a JSON configuration file")
	flag.Parse()

	cfg, err := app.LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "configuration:", err)
		os.Exit(2)
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	service, err := app.New(cfg, logger)
	if err != nil {
		logger.Error("startup failed", "error", err)
		os.Exit(1)
	}

	addresses := append([]string{cfg.Listen}, cfg.ExtraListen...)
	servers := make([]*http.Server, 0, len(addresses))
	for _, address := range addresses {
		servers = append(servers, &http.Server{
			Addr:              address,
			Handler:           service.Handler(),
			ReadHeaderTimeout: 10 * time.Second,
			IdleTimeout:       90 * time.Second,
		})
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	service.StartIMSPreferenceReconciler(ctx)
	service.StartSMSDeliveryReconciler(ctx)
	if err := service.StartRTPProxy(ctx); err != nil {
		logger.Error("RTP proxy startup failed", "error", err)
		os.Exit(1)
	}
	errCh := make(chan error, len(servers))
	for _, server := range servers {
		logger.Info("server listening", "address", server.Addr, "provider", cfg.Telephony.Type,
			"sim_auth", cfg.SIMAuth.Type)
		go func(server *http.Server) {
			if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				errCh <- err
			}
		}(server)
	}

	select {
	case <-ctx.Done():
	case err := <-errCh:
		logger.Error("server stopped", "error", err)
		stop()
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, server := range servers {
		_ = server.Shutdown(shutdownCtx)
	}
}
