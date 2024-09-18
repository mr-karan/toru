package main

import (
	"fmt"
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

	Cache struct {
		Enabled bool   `koanf:"enabled"`
		Type    string `koanf:"type"`
		S3      struct {
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
		// Enabled is a flag to enable or disable the auth module.
		Enabled bool `koanf:"enabled"`

		// Modules is a list of auth modules.
		Modules []AuthModule `koanf:"modules"`
	} `koanf:"auth"`
}

// AuthModule represents an auth module configuration.
// Auth modules are used to authenticate users.
// The auth module implementation is determined by the Type field.
type AuthModule struct {
	// Name of the auth module. This is used to identify the module via
	// the username in basic auth.
	Name string `koanf:"name"`

	// Type of the auth module. This is used to identify the module
	// implementation.
	Type string `koanf:"type"`

	// Options is a map of options specific to the auth module. These
	// are used to configure the auth module.
	Options map[string]interface{} `koanf:"options"`
}

// initConfig loads config and returns a Config instance.
func initConfig(cfgDefault, envPrefix string) (*Config, error) {
	var (
		ko = koanf.New(".")
		f  = flag.NewFlagSet("app", flag.ContinueOnError)
	)

	// Configure Flags.
	f.Usage = func() {
		fmt.Println(f.FlagUsages())
		os.Exit(0)
	}

	// Register flags.
	f.String("config", cfgDefault, "Path to a config file to load.")

	// Parse and Load Flags.
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

	return cfg, nil
}
