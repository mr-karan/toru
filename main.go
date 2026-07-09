package main

import (
	"log/slog"
	"os"
)

var buildString = "dev"

func main() {
	cfg, err := initConfig("config.toml", "TORU_")
	if err != nil {
		slog.Error("Error initializing config", "error", err)
		os.Exit(1)
	}

	logger := setupLogger(cfg.Server.LogLevel)
	if err := run(cfg, logger); err != nil {
		logger.Error("Server error", "error", err)
		os.Exit(1)
	}
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

	handler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel})
	return slog.New(handler).With("service", "toru")
}
