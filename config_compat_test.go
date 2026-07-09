package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// Verification contract:
// 1. Legacy Go-only config must continue to parse.
// 2. Legacy config must normalize into explicit Go runtime semantics.
// 3. New multi-listener config must decode into actual listener/protocol values.

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

	listeners := requireField(t, reflect.ValueOf(*cfg), "Listeners")
	if listeners.Len() != 1 {
		t.Fatalf("legacy config must synthesize exactly one Go listener, got %d", listeners.Len())
	}

	listener := listeners.Index(0)
	address := stringField(t, listener, "Address")
	if address != ":8888" {
		t.Fatalf("legacy listener address = %q, want %q", address, ":8888")
	}

	protocols := stringSliceField(t, listener, "Protocols")
	if len(protocols) != 1 || protocols[0] != "go" {
		t.Fatalf("legacy listener protocols = %v, want [go]", protocols)
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
	root := reflect.ValueOf(*cfg)

	listeners := requireField(t, root, "Listeners")
	if listeners.Len() != 2 {
		t.Fatalf("expected 2 listeners from config, got %d", listeners.Len())
	}

	goListener := listeners.Index(0)
	if got := stringField(t, goListener, "Name"); got != "go-public" {
		t.Fatalf("go listener name = %q, want %q", got, "go-public")
	}
	if got := stringField(t, goListener, "Address"); got != ":8080" {
		t.Fatalf("go listener address = %q, want %q", got, ":8080")
	}
	if got := stringSliceField(t, goListener, "Protocols"); len(got) != 1 || got[0] != "go" {
		t.Fatalf("go listener protocols = %v, want [go]", got)
	}

	npmListener := listeners.Index(1)
	if got := stringField(t, npmListener, "Name"); got != "npm-public" {
		t.Fatalf("npm listener name = %q, want %q", got, "npm-public")
	}
	if got := stringField(t, npmListener, "Address"); got != ":8081" {
		t.Fatalf("npm listener address = %q, want %q", got, ":8081")
	}
	if got := stringSliceField(t, npmListener, "Protocols"); len(got) != 1 || got[0] != "npm" {
		t.Fatalf("npm listener protocols = %v, want [npm]", got)
	}

	protocols := requireField(t, root, "Protocols")
	npmProtocol := requireField(t, protocols, "NPM")
	if !boolField(t, npmProtocol, "Enabled") {
		t.Fatalf("protocols.npm.enabled must decode to true")
	}
	if got := stringField(t, npmProtocol, "Upstream"); got != "https://registry.npmjs.org" {
		t.Fatalf("protocols.npm.upstream = %q, want npm registry upstream", got)
	}
	if got := stringField(t, npmProtocol, "BaseURL"); got != "https://npm-toru.example.com" {
		t.Fatalf("protocols.npm.base_url = %q, want %q", got, "https://npm-toru.example.com")
	}
}

func TestMultiProtocolListenerRejectedUntilHostDispatchExists(t *testing.T) {
	cfgText := `[server]
address = ":9999"
log_level = "info"

[[listeners]]
name = "mixed"
address = ":8080"
protocols = ["go", "npm"]
hosts = ["toru.example.com", "npm-toru.example.com"]
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
		t.Fatalf("expected multi-protocol same-listener config to be rejected until host dispatch exists")
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

func requireField(t *testing.T, v reflect.Value, name string) reflect.Value {
	t.Helper()
	if v.Kind() == reflect.Pointer {
		v = v.Elem()
	}
	field := v.FieldByName(name)
	if !field.IsValid() {
		t.Fatalf("expected field %q to exist", name)
	}
	return field
}

func stringField(t *testing.T, v reflect.Value, name string) string {
	t.Helper()
	field := requireField(t, v, name)
	if field.Kind() != reflect.String {
		t.Fatalf("field %q must be string, got %s", name, field.Kind())
	}
	return field.String()
}

func boolField(t *testing.T, v reflect.Value, name string) bool {
	t.Helper()
	field := requireField(t, v, name)
	if field.Kind() != reflect.Bool {
		t.Fatalf("field %q must be bool, got %s", name, field.Kind())
	}
	return field.Bool()
}

func stringSliceField(t *testing.T, v reflect.Value, name string) []string {
	t.Helper()
	field := requireField(t, v, name)
	if field.Kind() != reflect.Slice {
		t.Fatalf("field %q must be slice, got %s", name, field.Kind())
	}
	out := make([]string, field.Len())
	for i := 0; i < field.Len(); i++ {
		if field.Index(i).Kind() != reflect.String {
			t.Fatalf("field %q must be []string", name)
		}
		out[i] = field.Index(i).String()
	}
	return out
}
