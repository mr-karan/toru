package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/goproxy/goproxy"
)

type Proxy struct {
	client         *goproxy.Goproxy
	cfg            *Config
	logger         *slog.Logger
	server         *http.Server
	authenticators map[string]Authenticator
}

func newProxy(cfg *Config, logger *slog.Logger) (*Proxy, error) {
	// Set up custom transport with timeouts
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{
		Timeout:   30 * time.Second, // Connection timeout
		KeepAlive: 30 * time.Second,
	}).DialContext

	fetcher, err := newFetcher(cfg, logger)
	if err != nil {
		return nil, fmt.Errorf("failed to create fetcher: %w", err)
	}

	var cacher goproxy.Cacher
	if cfg.Cache.Enabled {
		switch cfg.Cache.Type {
		case "s3":
			cacher, err = newS3Cacher(cfg)
		case "disk":
			cacher = goproxy.DirCacher(cfg.Cache.Disk.Path)
		default:
			return nil, fmt.Errorf("unsupported cache type: %s", cfg.Cache.Type)
		}
		if err != nil {
			return nil, fmt.Errorf("failed to create cacher: %w", err)
		}
	}

	client := &goproxy.Goproxy{
		Fetcher:   fetcher,
		Cacher:    cacher,
		Transport: transport,
	}

	handler := http.Handler(client)

	// Add fetch timeout middleware if configured
	if cfg.Server.FetchTimeout > 0 {
		handler = createTimeoutMiddleware(handler, cfg.Server.FetchTimeout)
	}

	server := &http.Server{
		Addr:    cfg.Server.Address,
		Handler: handler,
		BaseContext: func(_ net.Listener) context.Context {
			return context.Background()
		},
	}

	authenticators := make(map[string]Authenticator)
	if cfg.Auth.Enabled {
		for _, module := range cfg.Auth.Modules {
			auth, err := NewAuthenticator(module)
			if err != nil {
				return nil, fmt.Errorf("failed to create authenticator: %w", err)
			}
			authenticators[module.Name] = auth
		}
	}

	return &Proxy{
		client:         client,
		cfg:            cfg,
		logger:         logger,
		server:         server,
		authenticators: authenticators,
	}, nil
}

func createTimeoutMiddleware(next http.Handler, timeout time.Duration) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	requestsTotal.Inc()
	startTime := time.Now()

	p.logger.Info("Received request",
		"method", r.Method,
		"path", r.URL.Path,
		"remote_addr", r.RemoteAddr,
	)

	if p.cfg.Auth.Enabled {
		// Extract credentials from the request
		authMethod, password, ok := r.BasicAuth()
		if !ok {
			http.Error(w, "No username or password provided", http.StatusUnauthorized)
			return
		}

		// Check if the module is enabled
		auth, ok := p.authenticators[authMethod]
		if !ok {
			http.Error(w, "Invalid auth method", http.StatusBadRequest)
			return
		}

		// Check if the credentials are valid
		skip, hasAccess, err := auth.Authenticate(password, r.URL.Path)
		if err != nil {
			p.logger.Error("Failed to authenticate", "error", err)
			if err == ErrorAuthFailed {
				http.Error(w, "Unauthorized: Invalid access token or insufficient permissions for the GitLab project", http.StatusForbidden)
				return
			}
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}

		if !hasAccess && !skip {
			http.Error(w, "Unauthorized: Invalid access token or insufficient permissions for the GitLab project", http.StatusForbidden)
			return
		}
	}

	// Wrap the ResponseWriter to capture the response size
	rw := &responseWriter{ResponseWriter: w}

	p.client.ServeHTTP(rw, r)

	requestDuration.UpdateDuration(startTime)
	responseSize.Update(float64(rw.size))
}

// responseWriter wraps http.ResponseWriter to capture the response size
type responseWriter struct {
	http.ResponseWriter
	size int
}

func (rw *responseWriter) Write(b []byte) (int, error) {
	n, err := rw.ResponseWriter.Write(b)
	rw.size += n
	return n, err
}

func (p *Proxy) ListenAndServe() error {
	p.logger.Info("Starting Go module proxy", "address", p.cfg.Server.Address)
	return p.server.ListenAndServe()
}

func (p *Proxy) Shutdown(ctx context.Context) error {
	p.logger.Info("Shutting down server gracefully...")
	return p.server.Shutdown(ctx)
}
