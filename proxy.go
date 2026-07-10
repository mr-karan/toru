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
	proxyLogger := logger.With("component", "proxy")
	fetcherLogger := logger.With("component", "fetcher")
	cacheLogger := logger.With("component", "cache")

	// Set up custom transport with timeouts
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{
		Timeout:   30 * time.Second, // Connection timeout
		KeepAlive: 30 * time.Second,
	}).DialContext

	fetcher, err := newFetcher(cfg, fetcherLogger, transport)
	if err != nil {
		return nil, fmt.Errorf("failed to create fetcher: %w", err)
	}

	var cacher goproxy.Cacher
	if cfg.Cache.Enabled {
		switch cfg.Cache.Type {
		case "s3":
			cacher, err = newS3Cacher(cfg, cacheLogger.With("cache_type", "s3"))
		case "disk":
			cacher = newDiskCacher(cfg.Cache.Disk.Path, cacheLogger.With("cache_type", "disk"))
		default:
			return nil, fmt.Errorf("unsupported cache type: %s", cfg.Cache.Type)
		}
		if err != nil {
			return nil, fmt.Errorf("failed to create cacher: %w", err)
		}
		cacher = newMetadataCacher(cacher, cfg.Cache.MutableMetadataTTL, cacheLogger)
	}

	client := &goproxy.Goproxy{
		Fetcher:   fetcher,
		Cacher:    cacher,
		Logger:    logger.With("component", "goproxy"),
		Transport: transport,
	}

	handler := http.Handler(client)

	// NOTE: FetchTimeout is intentionally NOT applied as whole-handler middleware.
	// A request-wide deadline also covers the post-200 client copy, so a slow
	// cold-cache build that overran the deadline was aborted mid-stream and
	// surfaced to the `go` client as an HTTP/2 INTERNAL_ERROR (stream reset)
	// instead of a retryable error. The timeout is now scoped to the upstream
	// fetch/build phase inside the fetcher (see fetcher.withFetchTimeout), which
	// completes before any response headers are written.

	server := &http.Server{
		Addr:    cfg.Server.Address,
		Handler: handler,
		BaseContext: func(_ net.Listener) context.Context {
			return context.Background()
		},
	}

	authenticators, err := buildAuthenticators(cfg)
	if err != nil {
		return nil, err
	}

	return &Proxy{
		client:         client,
		cfg:            cfg,
		logger:         proxyLogger,
		server:         server,
		authenticators: authenticators,
	}, nil
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
		authMethod, _, ok := r.BasicAuth()
		if !ok {
			http.Error(w, "No username or password provided", http.StatusUnauthorized)
			return
		}
		if !authorizeRequest(w, r, p.authenticators, authMethod, AuthRequest{Protocol: "go", Path: r.URL.Path, Resource: r.URL.Path}) {
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
