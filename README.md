<a href="https://zerodha.tech"><img src="https://zerodha.tech/static/images/github-badge.svg" align="right" /></a>

# toru

_Toru is a multi-protocol dependency proxy. It started as a Go module proxy on top of [goproxy/goproxy](https://github.com/goproxy/goproxy) and now also supports npm registry read/install flows compatible with pnpm._

## Features

- Go module proxy protocol support
- npm registry read/install support for npm and pnpm
- Single process, single config, multi-listener runtime
- Disk and S3-backed caching for Go artifacts
- Disk-backed npm metadata caching and conditional tarball caching when `cache.enabled = true`, `cache.type = "disk"`, and `cache.disk.path` is set
- Go vanity import path rewrites
- npm tarball URL rewriting back to Toru
- Protocol-aware auth hooks for protected Go paths and npm scopes
- Prometheus-compatible metrics endpoint on every listener

## Current scope

Implemented today:
- Go proxying with existing rewrite and cache behavior
- npm package metadata requests
- npm tarball requests
- scoped and unscoped npm packages
- npm metadata TTL cache on disk
- protected npm scopes via auth modules

Not implemented yet:
- npm publish APIs
- host-based multi-protocol dispatch on a single listener

For now, if you want both protocols in one process, run separate listeners, one protocol per listener.

## Running with Docker

```bash
docker run --rm -p 8888:8888 -p 8889:8889 ghcr.io/mr-karan/toru:latest
```

## Configuration

Toru is configured via TOML. See [config.sample.toml](./config.sample.toml) for a complete example.

### Legacy Go-only mode

If you omit `[[listeners]]` and `protocols.*`, Toru preserves the legacy Go-only behavior and serves the Go proxy on `server.address`.

```toml
[server]
address = ":8888"
log_level = "info"
fetch_timeout = "30s"

[cache]
enabled = true
type = "disk"

[cache.disk]
path = "/tmp/toru-cache"
```

### Multi-listener mode

```toml
[server]
log_level = "info"
fetch_timeout = "30s"

[[listeners]]
name = "go"
address = ":8888"
protocols = ["go"]

[[listeners]]
name = "npm"
address = ":8889"
protocols = ["npm"]

[cache]
enabled = true
type = "disk"

[cache.disk]
path = "/tmp/toru-cache"

[protocols.go]
enabled = true
fetch_timeout = "30s"

[protocols.npm]
enabled = true
upstream = "https://registry.npmjs.org"
metadata_ttl = "5m"
base_url = "http://127.0.0.1:8889"
```

## Go mode

### Rewrite rules

Toru supports rewrite rules that map vanity import paths to their actual repository locations.

```toml
[[rewrite_rules]]
vanity_path = "go.example.com"
target_path = "gitlab.example.com"
```

With this rule:
1. clients request `go.example.com/awesome-pkg`
2. Toru rewrites to `gitlab.example.com/awesome-pkg`
3. Toru fetches from the actual repository location

### Mutable metadata caching

Toru treats `/@v/list`, `@latest`, and non-canonical `.info` lookups as mutable metadata.

```toml
[cache]
mutable_metadata_ttl = "0s"
```

Set `cache.mutable_metadata_ttl` to a non-zero duration if you want short-lived persistent caching for mutable Go metadata while still caching immutable `.mod` and `.zip` artifacts normally.

### Go auth

Go auth continues to work through auth modules configured under `[auth]`.

Example:

```toml
[auth]
enabled = true

[[auth.modules]]
name = "gitlab"
type = "gitlab_access_token"
options.root_url = "https://gitlab.example.com"
options.protected_uri = "go.example.com"
```

Client example:

```bash
export GOPROXY=https://gitlab:<access_token>@toru.example.com:9443
```

## npm / pnpm mode

### Registry behavior

Toru serves npm metadata, rewrites every `dist.tarball` URL back to Toru, fetches tarballs from upstream, and uses disk-backed tarball caching only when all of these are true:
- `cache.enabled = true`
- `cache.type = "disk"`
- `cache.disk.path` is set

### npm metadata cache

npm metadata uses a TTL cache on disk.

```toml
[protocols.npm]
metadata_ttl = "5m"
```

The npm metadata cache is only active when all of these are true:
- `cache.enabled = true`
- `cache.type = "disk"`
- `cache.disk.path` is set
- `protocols.npm.metadata_ttl > 0`

If any of those are false, npm metadata is always fetched fresh from upstream.

### npm protected scopes

You can require auth for specific npm scopes.

```toml
[auth]
enabled = true

[[auth.modules]]
name = "static"
type = "static_token"
options.token = "secret"

[protocols.npm]
enabled = true
upstream = "https://registry.npmjs.org"
metadata_ttl = "5m"
base_url = "http://127.0.0.1:8889"

[[protocols.npm.protected_scopes]]
scope = "@toru"
auth_module = "static"
```

Accepted auth forms for protected npm scopes:
- Basic auth, where username is the auth module name and password is the token
- `Authorization: Bearer <token>`

### Client usage

pnpm:

```bash
pnpm config set registry http://127.0.0.1:8889
pnpm add is-number
```

npm:

```bash
npm config set registry http://127.0.0.1:8889
npm install is-number
```

Protected-scope example with bearer token in `.npmrc`:

```ini
registry=http://127.0.0.1:8889/
//127.0.0.1:8889/:_authToken=secret
always-auth=true
```

## Front-proxy routing examples

Current deployment model is one Toru process with separate listeners per protocol.

HAProxy port split example:

```haproxy
frontend deps
    bind *:443 ssl crt /etc/ssl/example.pem
    use_backend toru_go if { hdr(host) -i go.example.com }
    use_backend toru_npm if { hdr(host) -i npm.example.com }

backend toru_go
    server toru-go 127.0.0.1:8888

backend toru_npm
    server toru-npm 127.0.0.1:8889
```

Toru itself does not yet dispatch Go and npm traffic by host on a single listener. Keep one protocol per listener and let the front proxy route by host or port.

## Local development

Build and run:

```bash
just build
just run
```

Or use the config file directly:

```bash
go run . --config config.sample.toml
```

## Real-client smoke commands

Go:

```bash
GOPROXY=http://127.0.0.1:8888 go mod download rsc.io/quote@v1.5.2
```

pnpm:

```bash
pnpm config set registry http://127.0.0.1:8889
pnpm add is-number
```

## Metrics

Toru exposes Prometheus-compatible metrics at `/metrics`.

Current metric set includes the legacy aggregate counters plus protocol-aware additions.

Legacy metrics retained for compatibility:

```
toru_requests_total
toru_request_duration_seconds
toru_upstream_fetch_duration_seconds
toru_response_size_bytes
toru_rewrite_rules_applied_total
toru_errors_total
toru_cache_hits_total
toru_cache_misses_total
toru_cache_writes_total
toru_cache_errors_total
```

New protocol-aware metrics include:

```
toru_requests_by_protocol_total{protocol,kind}
toru_request_duration_by_protocol_seconds{protocol,kind}
toru_response_size_by_protocol_bytes{protocol,kind}
toru_cache_hits_by_protocol_total{protocol,class}
toru_cache_misses_by_protocol_total{protocol,class}
toru_cache_writes_by_protocol_total{protocol,class}
toru_cache_errors_by_protocol_total{protocol,class}
```
