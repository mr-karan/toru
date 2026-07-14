package main

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	flag "github.com/spf13/pflag"

	"github.com/knadh/koanf/parsers/toml"
	"github.com/knadh/koanf/providers/env"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/providers/posflag"
	"github.com/knadh/koanf/v2"
)

type Config struct {
	Server struct {
		Address      string        `koanf:"address"`
		LogLevel     string        `koanf:"log_level"`
		FetchTimeout time.Duration `koanf:"fetch_timeout"`
	} `koanf:"server"`

	Listeners []ListenerConfig `koanf:"listeners"`
	Protocols ProtocolsConfig  `koanf:"protocols"`

	Cache struct {
		Enabled            bool          `koanf:"enabled"`
		Type               string        `koanf:"type"`
		MutableMetadataTTL time.Duration `koanf:"mutable_metadata_ttl"`
		S3                 struct {
			Region    string `koanf:"region"`
			Bucket    string `koanf:"bucket"`
			AccessKey string `koanf:"access_key"`
			SecretKey string `koanf:"secret_key"`
		} `koanf:"s3"`
		Disk struct {
			Path string `koanf:"path"`
		} `koanf:"disk"`
	} `koanf:"cache"`

	RewriteRules []struct {
		VanityPath string `koanf:"vanity_path"`
		TargetPath string `koanf:"target_path"`
	} `koanf:"rewrite_rules"`

	Auth struct {
		Enabled bool         `koanf:"enabled"`
		Modules []AuthModule `koanf:"modules"`
	} `koanf:"auth"`
}

type ListenerConfig struct {
	Name      string   `koanf:"name"`
	Address   string   `koanf:"address"`
	Protocols []string `koanf:"protocols"`
	Hosts     []string `koanf:"hosts"`
}

type ProtocolsConfig struct {
	Go  GoProtocolConfig  `koanf:"go"`
	NPM NPMProtocolConfig `koanf:"npm"`
}

type GoProtocolConfig struct {
	Enabled      bool          `koanf:"enabled"`
	FetchTimeout time.Duration `koanf:"fetch_timeout"`
}

type NPMProtocolConfig struct {
	Enabled         bool                 `koanf:"enabled"`
	Upstream        string               `koanf:"upstream"`
	MetadataTTL     time.Duration        `koanf:"metadata_ttl"`
	BaseURL         string               `koanf:"base_url"`
	ProtectedScopes []ProtectedScopeRule `koanf:"protected_scopes"`
	RewriteRules    []NPMRewriteRule     `koanf:"rewrite_rules"`
}

type ProtectedScopeRule struct {
	Scope      string `koanf:"scope"`
	AuthModule string `koanf:"auth_module"`
}

// AuthModule represents an auth module configuration.
type AuthModule struct {
	Name    string                 `koanf:"name"`
	Type    string                 `koanf:"type"`
	Options map[string]interface{} `koanf:"options"`
}

func initConfig(cfgDefault, envPrefix string) (*Config, error) {
	var (
		ko = koanf.New(".")
		f  = flag.NewFlagSet("app", flag.ContinueOnError)
	)

	f.Usage = func() {
		fmt.Println(f.FlagUsages())
		os.Exit(0)
	}
	f.String("config", cfgDefault, "Path to a config file to load.")

	err := f.Parse(os.Args[1:])
	if err != nil {
		return nil, err
	}
	if err := ko.Load(posflag.Provider(f, ".", ko), nil); err != nil {
		return nil, err
	}
	if err := ko.Load(file.Provider(ko.String("config")), toml.Parser()); err != nil {
		return nil, err
	}
	if err := ko.Load(env.Provider(envPrefix, ".", func(s string) string {
		return strings.Replace(strings.ToLower(strings.TrimPrefix(s, envPrefix)), "__", ".", -1)
	}), nil); err != nil {
		return nil, err
	}

	cfg := &Config{}
	if err := ko.Unmarshal("", cfg); err != nil {
		return nil, err
	}
	cfg.normalize()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) normalize() {
	if c.Protocols.Go.FetchTimeout == 0 {
		c.Protocols.Go.FetchTimeout = c.Server.FetchTimeout
	}
	if len(c.Listeners) == 0 {
		c.Protocols.Go.Enabled = true
		c.Listeners = []ListenerConfig{{
			Name:      "go",
			Address:   c.Server.Address,
			Protocols: []string{"go"},
		}}
	}
	if !c.Protocols.Go.Enabled && !c.Protocols.NPM.Enabled {
		for _, l := range c.Listeners {
			for _, p := range l.Protocols {
				switch p {
				case "go":
					c.Protocols.Go.Enabled = true
				case "npm":
					c.Protocols.NPM.Enabled = true
				}
			}
		}
	}
	if c.Protocols.NPM.Upstream == "" {
		c.Protocols.NPM.Upstream = "https://registry.npmjs.org"
	}
}

func (c *Config) validate() error {
	if c.Protocols.NPM.Enabled {
		if strings.TrimSpace(c.Protocols.NPM.BaseURL) == "" {
			return fmt.Errorf("protocols.npm.base_url is required when npm protocol is enabled")
		}
		parsedBaseURL, err := url.Parse(c.Protocols.NPM.BaseURL)
		if err != nil || parsedBaseURL.Scheme == "" || parsedBaseURL.Host == "" {
			return fmt.Errorf("protocols.npm.base_url must be an absolute URL")
		}
		if err := validateNPMRewriteRules(c.Protocols.NPM); err != nil {
			return err
		}
	}
	for _, listener := range c.Listeners {
		if len(listener.Protocols) == 0 {
			return fmt.Errorf("listener %q must declare at least one protocol", listener.Name)
		}
		if len(listener.Protocols) == 1 {
			continue
		}
		if len(listener.Hosts) != len(listener.Protocols) {
			return fmt.Errorf("listener %q must declare one host per protocol for host dispatch", listener.Name)
		}
		seenProtocols := map[string]struct{}{}
		seenHosts := map[string]struct{}{}
		for i, protocol := range listener.Protocols {
			if _, ok := seenProtocols[protocol]; ok {
				return fmt.Errorf("listener %q declares duplicate protocol %q in host dispatch config", listener.Name, protocol)
			}
			seenProtocols[protocol] = struct{}{}
			host := normalizeListenerHost(listener.Hosts[i])
			if host == "" {
				return fmt.Errorf("listener %q has empty host for protocol %q", listener.Name, protocol)
			}
			if _, ok := seenHosts[host]; ok {
				return fmt.Errorf("listener %q declares duplicate host %q", listener.Name, host)
			}
			seenHosts[host] = struct{}{}
		}
	}
	return nil
}

func normalizeListenerHost(host string) string {
	host = strings.TrimSpace(strings.ToLower(host))
	if host == "" {
		return ""
	}
	if parsedHost, _, err := net.SplitHostPort(host); err == nil {
		return parsedHost
	}
	return host
}
