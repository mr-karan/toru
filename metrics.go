package main

import "github.com/VictoriaMetrics/metrics"

var (
	// Total number of requests
	requestsTotal = metrics.NewCounter("toru_requests_total")

	// Request duration
	requestDuration = metrics.NewSummary("toru_request_duration_seconds")

	// Upstream fetch duration
	upstreamFetchDuration = metrics.NewSummary("toru_upstream_fetch_duration_seconds")

	// Response size
	responseSize = metrics.NewHistogram("toru_response_size_bytes")

	// Rewrite rules applied
	rewriteRulesApplied = metrics.NewCounter("toru_rewrite_rules_applied_total")

	// Errors encountered
	errorsTotal = metrics.NewCounter("toru_errors_total")
)
