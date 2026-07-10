package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Verification contract:
// 1. Legacy Go-only config must continue to parse.
// 2. Legacy config must normalize into explicit Go runtime semantics.
// 3. New multi-listener config must decode into actual listener/protocol values.
// 4. Same-listener host dispatch must require explicit host-to-protocol mapping.

func TestLegacyConfigStillParsesAsGoOnly(t *testing.T) {
	cfgText := `[server]
address = ":8888"
log_level = "info"
fetch_timeout = "30s"

[cache]
enabled = true
type = "disk"
mutable_metadata_ttl = "0s"

[cache.disk]
path = "/tmp/toru-cache"

[auth]
enabled = false

[[rewrite_rules]]
vanity_path = "example.com/mymodule"
target_path = "github.com/example/mymodule"
`

	cfg := loadConfigFromText(t, cfgText)

	if len(cfg.Listeners) != 1 {
		t.Fatalf("legacy config must synthesize exactly one Go listener, got %d", len(cfg.Listeners))
	}
	listener := cfg.Listeners[0]
	if listener.Name != "go" {
		t.Fatalf("legacy listener name = %q, want %q", listener.Name, "go")
	}
	if listener.Address != ":8888" {
		t.Fatalf("legacy listener address = %q, want %q", listener.Address, ":8888")
	}
	if len(listener.Protocols) != 1 || listener.Protocols[0] != "go" {
		t.Fatalf("legacy listener protocols = %v, want [go]", listener.Protocols)
	}
	if !cfg.Protocols.Go.Enabled {
		t.Fatalf("legacy config must normalize to go enabled")
	}
	if cfg.Protocols.Go.FetchTimeout != 30*time.Second {
		t.Fatalf("legacy go fetch timeout = %s, want 30s", cfg.Protocols.Go.FetchTimeout)
	}
}

func TestMultiListenerConfigParses(t *testing.T) {
	cfgText := `[server]
address = ":9999"
log_level = "info"

[[listeners]]
name = "go-public"
address = ":8080"
protocols = ["go"]
hosts = ["toru.example.com"]

[[listeners]]
name = "npm-public"
address = ":8081"
protocols = ["npm"]
hosts = ["npm-toru.example.com"]

[cache]
enabled = true
type = "disk"
mutable_metadata_ttl = "0s"

[cache.disk]
path = "/tmp/toru-cache"

[protocols.go]
enabled = true
fetch_timeout = "30s"

[protocols.npm]
enabled = true
upstream = "https://registry.npmjs.org"
metadata_ttl = "5m"
base_url = "https://npm-toru.example.com"
`

	cfg := loadConfigFromText(t, cfgText)
	if len(cfg.Listeners) != 2 {
		t.Fatalf("expected 2 listeners from config, got %d", len(cfg.Listeners))
	}

	goListener := cfg.Listeners[0]
	if goListener.Name != "go-public" {
		t.Fatalf("go listener name = %q, want %q", goListener.Name, "go-public")
	}
	if goListener.Address != ":8080" {
		t.Fatalf("go listener address = %q, want %q", goListener.Address, ":8080")
	}
	if len(goListener.Protocols) != 1 || goListener.Protocols[0] != "go" {
		t.Fatalf("go listener protocols = %v, want [go]", goListener.Protocols)
	}
	if len(goListener.Hosts) != 1 || goListener.Hosts[0] != "toru.example.com" {
		t.Fatalf("go listener hosts = %v, want [toru.example.com]", goListener.Hosts)
	}

	npmListener := cfg.Listeners[1]
	if npmListener.Name != "npm-public" {
		t.Fatalf("npm listener name = %q, want %q", npmListener.Name, "npm-public")
	}
	if npmListener.Address != ":8081" {
		t.Fatalf("npm listener address = %q, want %q", npmListener.Address, ":8081")
	}
	if len(npmListener.Protocols) != 1 || npmListener.Protocols[0] != "npm" {
		t.Fatalf("npm listener protocols = %v, want [npm]", npmListener.Protocols)
	}
	if len(npmListener.Hosts) != 1 || npmListener.Hosts[0] != "npm-toru.example.com" {
		t.Fatalf("npm listener hosts = %v, want [npm-toru.example.com]", npmListener.Hosts)
	}
	if !cfg.Protocols.NPM.Enabled {
		t.Fatalf("protocols.npm.enabled must decode to true")
	}
	if cfg.Protocols.NPM.Upstream != "https://registry.npmjs.org" {
		t.Fatalf("protocols.npm.upstream = %q, want npm registry upstream", cfg.Protocols.NPM.Upstream)
	}
	if cfg.Protocols.NPM.BaseURL != "https://npm-toru.example.com" {
		t.Fatalf("protocols.npm.base_url = %q, want %q", cfg.Protocols.NPM.BaseURL, "https://npm-toru.example.com")
	}
}

func TestSingleListenerHostDispatchConfigParses(t *testing.T) {
	cfgText := `[server]
address = ":9999"
log_level = "info"

[[listeners]]
name = "shared"
address = ":8080"
protocols = ["go", "npm"]
hosts = ["toru.example.com", "npm-toru.example.com"]

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
base_url = "https://npm-toru.example.com"
`

	cfg := loadConfigFromText(t, cfgText)
	if len(cfg.Listeners) != 1 {
		t.Fatalf("expected 1 shared listener, got %d", len(cfg.Listeners))
	}
	listener := cfg.Listeners[0]
	if listener.Name != "shared" {
		t.Fatalf("listener name = %q, want shared", listener.Name)
	}
	if len(listener.Protocols) != 2 || listener.Protocols[0] != "go" || listener.Protocols[1] != "npm" {
		t.Fatalf("listener protocols = %v, want [go npm]", listener.Protocols)
	}
	if len(listener.Hosts) != 2 || listener.Hosts[0] != "toru.example.com" || listener.Hosts[1] != "npm-toru.example.com" {
		t.Fatalf("listener hosts = %v, want [toru.example.com npm-toru.example.com]", listener.Hosts)
	}
}

func TestSingleListenerHostDispatchRejectsAmbiguousConfig(t *testing.T) {
	cfgText := `[server]
address = ":9999"
log_level = "info"

[[listeners]]
name = "mixed"
address = ":8080"
protocols = ["go", "npm"]
hosts = ["toru.example.com"]
`

	tmp := t.TempDir()
	path := filepath.Join(tmp, "config.toml")
	if err := os.WriteFile(path, []byte(cfgText), 0o644); err != nil {
		t.Fatalf("write temp config: %v", err)
	}

	oldArgs := os.Args
	defer func() { os.Args = oldArgs }()
	os.Args = []string{"toru", "--config", path}

	_, err := initConfig(path, "TORU_")
	if err == nil {
		t.Fatalf("expected ambiguous same-listener host-dispatch config to be rejected")
	}
}

func loadConfigFromText(t *testing.T, content string) *Config {
	t.Helper()
	tmp := t.TempDir()
	path := filepath.Join(tmp, "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write temp config: %v", err)
	}

	oldArgs := os.Args
	defer func() { os.Args = oldArgs }()
	os.Args = []string{"toru", "--config", path}

	cfg, err := initConfig(path, "TORU_")
	if err != nil {
		t.Fatalf("initConfig() error = %v", err)
	}
	return cfg
}
