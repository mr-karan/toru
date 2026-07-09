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
		if len(listener.Protocols) != 1 {
			return nil, fmt.Errorf("listener %q must declare exactly one protocol until host dispatch is implemented", listener.Name)
		}

		mux := http.NewServeMux()
		mux.HandleFunc("/metrics", func(w http.ResponseWriter, req *http.Request) {
			metrics.WritePrometheus(w, true)
		})

		switch listener.Protocols[0] {
		case "go":
			mux.Handle("/", goProxy)
		case "npm":
			mux.Handle("/", newNPMHandler(cfg, logger))
		default:
			return nil, fmt.Errorf("unsupported protocol: %s", listener.Protocols[0])
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
