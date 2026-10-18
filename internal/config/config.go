// Package config resolves the connection settings for a weavectl
// invocation. Precedence, highest first:
//
//  1. --server / --api-key / --context flags
//  2. WEAVECTL_SERVER / WEAVECTL_API_KEY environment variables
//  3. the context named by current-context in the config file
//  4. built-in default (http://localhost:8080)
package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

const (
	EnvServer = "WEAVECTL_SERVER"
	EnvAPIKey = "WEAVECTL_API_KEY"

	DefaultServer = "http://localhost:8080"
)

// Context is a named set of connection settings in the config file.
type Context struct {
	Server string `yaml:"server"`
	APIKey string `yaml:"api-key,omitempty"`
}

// File is the on-disk representation of ~/.weavectl/config.yaml.
type File struct {
	CurrentContext string             `yaml:"current-context"`
	Contexts       map[string]Context `yaml:"contexts"`
}

// Config is the resolved configuration for one invocation.
type Config struct {
	Server string
	APIKey string
}

// Options carries the command-line overrides.
type Options struct {
	ServerFlag   string
	APIKeyFlag   string
	ContextFlag  string
	ConfigPath   string
	WarnOut      io.Writer // receives warnings (e.g. unreadable config file); defaults to discarding
	NoConfigFile bool      // --config ignored entirely (tests)
}

// DefaultPath returns ~/.weavectl/config.yaml.
func DefaultPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".weavectl", "config.yaml"), nil
}

// Load resolves the effective configuration.
func Load(opts Options) (*Config, error) {
	cfg := &Config{Server: DefaultServer}

	if opts.NoConfigFile {
		applyOverrides(cfg, opts)
		return cfg, nil
	}

	path := opts.ConfigPath
	if path == "" {
		p, err := DefaultPath()
		if err != nil {
			return nil, fmt.Errorf("determine config path: %w", err)
		}
		path = p
	}

	f, err := loadFile(path)
	if err != nil {
		return nil, err
	}

	if f != nil {
		name := f.CurrentContext
		if opts.ContextFlag != "" {
			name = opts.ContextFlag
		}
		if name != "" {
			ctx, ok := f.Contexts[name]
			if !ok {
				return nil, fmt.Errorf("context %q not found in %s", name, path)
			}
			if ctx.Server != "" {
				cfg.Server = ctx.Server
			}
			cfg.APIKey = ctx.APIKey
		}
	}

	applyOverrides(cfg, opts)
	return cfg, nil
}

func applyOverrides(cfg *Config, opts Options) {
	if v := os.Getenv(EnvServer); v != "" {
		cfg.Server = v
	}
	if v := os.Getenv(EnvAPIKey); v != "" {
		cfg.APIKey = v
	}
	// Flags win over everything.
	if opts.ServerFlag != "" {
		cfg.Server = opts.ServerFlag
	}
	if opts.APIKeyFlag != "" {
		cfg.APIKey = opts.APIKeyFlag
	}
}

func loadFile(path string) (*File, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil // no config file is fine
		}
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	var f File
	if err := yaml.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	return &f, nil
}
