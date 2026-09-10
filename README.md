<a href="https://zerodha.tech"><img src="https://zerodha.tech/static/images/github-badge.svg" align="right" /></a>

# toru

_Toru is a Go module proxy with caching and rewrite capabilities, built on top of the [goproxy/goproxy](https://github.com/goproxy/goproxy) library._

Toru extends the functionality by adding features such as caching (S3) and configurable rewrite rules for module paths.

## Features

- Proxies Go module requests
- Supports caching (S3 and disk)
- Avoids persistently caching mutable module metadata like `/@v/list` and `@latest` by default
- Configurable rewrite rules for module paths
- Prometheus-compatible metrics endpoint

## Running with Docker

You can use the following command to run Toru using Docker:

```bash
docker run --rm -p 8888:8888 ghcr.io/mr-karan/toru:latest
```

## Rewrite Rules

Toru supports rewrite rules that allow you to map vanity import paths to their actual repository locations. This feature is particularly useful for organizations that want to use custom import paths for their Go packages.

### Example

Consider the following rewrite rule in your `config.toml`:

```toml
[[rewrite_rules]]
vanity_path = "go.example.com"
target_path = "gitlab.example.com"
```

With this rule in place:

1. When a user tries to fetch a package with the import path `go.example.com/awesome-pkg`, Toru intercepts this request.
2. Instead of looking for the package at `go.example.com/awesome-pkg`, Toru rewrites the request to `gitlab.example.com/awesome-pkg`.
3. Toru then fetches the package from the actual repository location at `gitlab.example.com/awesome-pkg`.

### Why is this useful?

1. **Vanity URLs**: You can use a clean, memorable vanity URL for your packages, making it easier for user to import them.
2. **Repository abstraction**: You can change the underlying repository location without affecting the import paths used by your user.
3. **Private repositories**: You can use rewrite rules to map public vanity URLs to private repository locations, allowing you to control access to your internal packages.

## Configuration

Toru can be configured using a TOML file and environment variables. Refer to [config.sample.toml](./config.sample.toml) for reference.

### Mutable metadata caching

Toru treats `/@v/list`, `@latest`, and non-canonical `.info` lookups as mutable metadata.

By default, persistent caching for that metadata is disabled in the sample config:

```toml
[cache]
mutable_metadata_ttl = "0s"
```

Set `cache.mutable_metadata_ttl` to a non-zero duration if you want short-lived persistent caching for mutable metadata while still caching immutable `.mod` and `.zip` artifacts normally.

## Local Dev

To build and run the project locally, use the provided `justfile`:

```bash
just build
just run
```

This will build the binary and run it with the default configuration file (`config.toml`).

## Metrics

Toru exposes Prometheus-compatible metrics at the `/metrics` endpoint. Available metrics include:

```
toru_requests_total: Total number of requests
toru_request_duration_seconds: Request duration
toru_upstream_fetch_duration_seconds: Upstream fetch duration
toru_response_size_bytes: Response size
toru_rewrite_rules_applied_total: Number of times rewrite rules were applied
toru_errors_total: Total number of errors encountered
toru_cache_hits_total: Total cache hits
toru_cache_misses_total: Total cache misses
toru_cache_writes_total: Total cache writes
toru_cache_errors_total: Total cache errors
```


## Authentication for private repositories

Toru can use separate credentials for upstream fetches and client access
checks. Keep these credentials separate.

### Upstream GitLab credential

Toru needs a server-side Git credential to fetch private modules on a cache
miss. For a GitLab fine-grained personal access token, grant:

- **Resource:** `Code`
- **Permission:** `Download`
- **Boundary:** only the projects or groups that contain the modules

The upstream credential does not need API, registry, or write permissions. Do
not put it in client configuration or commit it to the repository.

### For the Proxy

#### Using .netrc

Create or edit the `.netrc` file in your home directory:

```
machine gitlab.example.com
login your-username
password your-access-token
```

#### Using Git Configuration

Add this to your `.gitconfig`

```
[url "ssh://git@gitlab.example.com"]
	insteadOf = https://gitlab.example.com
```

### Client-side authentication

Toru provides support for client-side authentication through authentication modules.

To enable authentication, you can specify the modules in your configuration file:

```toml
[auth.modules]
name = "gitlab"
type = "gitlab_access_token"
options.root_url = "https://gitlab.example.com"
options.protected_uri = "go.example.com"
```

#### GitLab access token

Clients send a GitLab token as basic authentication. Toru uses that token to
call the GitLab project lookup API and check whether the client can access the
module project.

For a GitLab fine-grained personal access token, grant:

- **Resource:** `Project`
- **Permission:** `Read`
- **Boundary:** only the projects or groups that contain the modules

The client token only needs project-read access for this check. It does not
need the upstream `Code: Download` permission unless the same token also
fetches repositories outside Toru.

To authenticate using the access token, use the following command:

```bash
export TORU_TOKEN='<access-token>'
export GOPROXY="https://gitlab:${TORU_TOKEN}@toru.example.com:9443"
```
