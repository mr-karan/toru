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
	// Build a shared authenticator set for non-go handlers too.
	authenticators, err := buildAuthenticators(cfg)
	if err != nil {
		return nil, err
	}

	servers := make([]runtimeServer, 0, len(cfg.Listeners))
	npmHandler := newNPMHandler(cfg, logger, authenticators)
	for _, listener := range cfg.Listeners {
		mux := http.NewServeMux()
		mux.HandleFunc("/metrics", func(w http.ResponseWriter, req *http.Request) {
			metrics.WritePrometheus(w, true)
		})

		if len(listener.Protocols) == 1 {
			handler, err := protocolHandler(listener.Protocols[0], goProxy, npmHandler)
			if err != nil {
				return nil, err
			}
			mux.Handle("/", handler)
		} else {
			routes := make(map[string]http.Handler, len(listener.Protocols))
			for i, protocol := range listener.Protocols {
				handler, err := protocolHandler(protocol, goProxy, npmHandler)
				if err != nil {
					return nil, err
				}
				routes[normalizeListenerHost(listener.Hosts[i])] = handler
			}
			mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				handler, ok := routes[normalizeListenerHost(req.Host)]
				if !ok {
					http.Error(w, "unknown host for listener", http.StatusMisdirectedRequest)
					return
				}
				handler.ServeHTTP(w, req)
			}))
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

func protocolHandler(protocol string, goProxy, npmHandler http.Handler) (http.Handler, error) {
	switch protocol {
	case "go":
		return goProxy, nil
	case "npm":
		return npmHandler, nil
	default:
		return nil, fmt.Errorf("unsupported protocol: %s", protocol)
	}
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
