package main

import (
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"time"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	upstream := strings.TrimSpace(os.Getenv("VITI_PROXY_UPSTREAM"))
	if upstream == "" {
		upstream = "http://manager:8787"
	}
	target, err := url.Parse(upstream)
	if err != nil {
		logger.Error("invalid upstream", "error", err)
		os.Exit(2)
	}
	token := os.Getenv("VITI_API_KEY")
	if token == "" {
		logger.Error("VITI_API_KEY is required")
		os.Exit(2)
	}

	proxy := httputil.NewSingleHostReverseProxy(target)
	director := proxy.Director
	proxy.Director = func(request *http.Request) {
		director(request)
		request.Header.Set("Authorization", "Bearer "+token)
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, request *http.Request, err error) {
		logger.Warn("proxy request failed", "method", request.Method, "path", request.URL.Path, "error", err)
		http.Error(w, "manager unavailable", http.StatusBadGateway)
	}

	server := &http.Server{
		Addr:              "0.0.0.0:8788",
		Handler:           proxy,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       90 * time.Second,
	}
	logger.Info("trusted local proxy listening", "address", server.Addr, "upstream", target.String())
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Error("proxy stopped", "error", err)
		os.Exit(1)
	}
}
