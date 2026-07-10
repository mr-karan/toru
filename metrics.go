package main

import (
	"fmt"
	"time"

	"github.com/VictoriaMetrics/metrics"
)

var (
	// Legacy aggregate metrics
	requestsTotal         = metrics.NewCounter("toru_requests_total")
	requestDuration       = metrics.NewSummary("toru_request_duration_seconds")
	upstreamFetchDuration = metrics.NewSummary("toru_upstream_fetch_duration_seconds")
	responseSize          = metrics.NewHistogram("toru_response_size_bytes")
	rewriteRulesApplied   = metrics.NewCounter("toru_rewrite_rules_applied_total")
	errorsTotal           = metrics.NewCounter("toru_errors_total")
	cacheHits             = metrics.NewCounter("toru_cache_hits_total")
	cacheMisses           = metrics.NewCounter("toru_cache_misses_total")
	cacheWrites           = metrics.NewCounter("toru_cache_writes_total")
	cacheErrors           = metrics.NewCounter("toru_cache_errors_total")
)

func recordProtocolRequest(protocol, kind string, duration time.Duration, size int) {
	requestsTotal.Inc()
	requestDuration.UpdateDuration(time.Now().Add(-duration))
	responseSize.Update(float64(size))

	metrics.GetOrCreateCounter(fmt.Sprintf(`toru_requests_by_protocol_total{protocol=%q,kind=%q}`, protocol, kind)).Inc()
	metrics.GetOrCreateSummary(fmt.Sprintf(`toru_request_duration_by_protocol_seconds{protocol=%q,kind=%q}`, protocol, kind)).UpdateDuration(time.Now().Add(-duration))
	metrics.GetOrCreateHistogram(fmt.Sprintf(`toru_response_size_by_protocol_bytes{protocol=%q,kind=%q}`, protocol, kind)).Update(float64(size))
}

func recordProtocolCacheHit(protocol, class string) {
	cacheHits.Inc()
	metrics.GetOrCreateCounter(fmt.Sprintf(`toru_cache_hits_by_protocol_total{protocol=%q,class=%q}`, protocol, class)).Inc()
}

func recordProtocolCacheMiss(protocol, class string) {
	cacheMisses.Inc()
	metrics.GetOrCreateCounter(fmt.Sprintf(`toru_cache_misses_by_protocol_total{protocol=%q,class=%q}`, protocol, class)).Inc()
}

func recordProtocolCacheWrite(protocol, class string) {
	cacheWrites.Inc()
	metrics.GetOrCreateCounter(fmt.Sprintf(`toru_cache_writes_by_protocol_total{protocol=%q,class=%q}`, protocol, class)).Inc()
}

func recordProtocolCacheError(protocol, class string) {
	cacheErrors.Inc()
	metrics.GetOrCreateCounter(fmt.Sprintf(`toru_cache_errors_by_protocol_total{protocol=%q,class=%q}`, protocol, class)).Inc()
}
