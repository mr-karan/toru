package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/VictoriaMetrics/metrics"
)

func main() {
	// Initialize configuration
	cfg, err := initConfig("config.toml", "TORU_")
	if err != nil {
		slog.Error("Error initializing config", "error", err)
		os.Exit(1)
	}

	// Setup logger
	logger := setupLogger(cfg.Server.LogLevel)

	p, err := newProxy(cfg, logger)
	if err != nil {
		logger.Error("Failed to create proxy", "error", err)
		os.Exit(1)
	}

	// Create a new ServeMux
	mux := http.NewServeMux()

	// Add the metrics endpoint
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, req *http.Request) {
		metrics.WritePrometheus(w, true)
	})

	// Add the proxy handler for all other routes
	mux.Handle("/", p)

	server := &http.Server{
		Addr:    cfg.Server.Address,
		Handler: mux,
	}

	// Start the server in a goroutine
	go func() {
		logger.Info("Starting Go module proxy", "address", cfg.Server.Address)
		if err := server.ListenAndServe(); err != http.ErrServerClosed {
			logger.Error("Server error", "error", err)
		}
	}()

	// Wait for interrupt signal to gracefully shutdown the server
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	logger.Info("Shutting down server gracefully...")

	// Create a deadline to wait for.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Doesn't block if no connections, but will otherwise wait
	// until the timeout deadline.
	if err := server.Shutdown(ctx); err != nil {
		logger.Error("Server forced to shutdown", "error", err)
	}

	logger.Info("Server exited")
}

func setupLogger(level string) *slog.Logger {
	var logLevel slog.Level
	switch level {
	case "debug":
		logLevel = slog.LevelDebug
	case "info":
		logLevel = slog.LevelInfo
	case "warn":
		logLevel = slog.LevelWarn
	case "error":
		logLevel = slog.LevelError
	default:
		logLevel = slog.LevelInfo
	}

	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel}))
}
