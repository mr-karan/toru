package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/VictoriaMetrics/metrics"
)

type runtimeServer struct {
	name   string
	server *http.Server
}

func buildListeners(cfg *Config, logger *slog.Logger) ([]runtimeServer, error) {
	goProxy, err := newProxy(cfg, logger)
	if err != nil {
		return nil, err
	}

	servers := make([]runtimeServer, 0, len(cfg.Listeners))
	for _, listener := range cfg.Listeners {
		mux := http.NewServeMux()
		mux.HandleFunc("/metrics", func(w http.ResponseWriter, req *http.Request) {
			metrics.WritePrometheus(w, true)
		})

		for _, protocol := range listener.Protocols {
			switch protocol {
			case "go":
				mux.Handle("/", goProxy)
			case "npm":
				mux.Handle("/", newNPMHandler(cfg, logger))
			default:
				return nil, fmt.Errorf("unsupported protocol: %s", protocol)
			}
		}

		servers = append(servers, runtimeServer{
			name: listener.Name,
			server: &http.Server{
				Addr:    listener.Address,
				Handler: mux,
			},
		})
	}

	return servers, nil
}

func run(cfg *Config, logger *slog.Logger) error {
	servers, err := buildListeners(cfg, logger)
	if err != nil {
		return err
	}

	errCh := make(chan error, len(servers))
	for _, srv := range servers {
		srv := srv
		go func() {
			logger.Info("Starting listener", "name", srv.name, "address", srv.server.Addr)
			if err := srv.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				errCh <- err
			}
		}()
	}

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	select {
	case <-quit:
	case err := <-errCh:
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for _, srv := range servers {
		_ = srv.server.Shutdown(ctx)
	}
	return nil
}
