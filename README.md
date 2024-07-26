# toru

_Toru is a Go module proxy with caching and rewrite capabilities._

## Features

- Proxies Go module requests
- Supports caching (S3 and disk)
- Configurable rewrite rules for module paths
- Prometheus-compatible metrics endpoint

## Configuration

Toru can be configured using a TOML file and environment variables. Refer to [config.sample.toml](./config.sample.toml) for reference.

## Local Dev

To build the project, use the provided Makefile:

```
make build
make run
```

This will build the binary and run it with the default configuration file (config.toml).

## Metrics

Toru exposes Prometheus-compatible metrics at the `/metrics` endpoint. Available metrics include:

```
toru_requests_total: Total number of requests
toru_request_duration_seconds: Request duration
toru_upstream_fetch_duration_seconds: Upstream fetch duration
toru_response_size_bytes: Response size
toru_rewrite_rules_applied_total: Number of times rewrite rules were applied
toru_errors_total: Total number of errors encountered
```
