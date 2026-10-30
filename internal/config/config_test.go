package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// isolateEnv removes the WEAVECTL_* environment variables for the
// duration of the test so values from the developer's shell cannot
// leak into Load.
func isolateEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{EnvServer, EnvAPIKey} {
		if v, ok := os.LookupEnv(name); ok {
			t.Cleanup(func() { _ = os.Setenv(name, v) })
			_ = os.Unsetenv(name)
		}
	}
}

func TestLoadDefaults(t *testing.T) {
	isolateEnv(t)
	cfg, err := Load(Options{NoConfigFile: true})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server != DefaultServer || cfg.APIKey != "" {
		t.Fatalf("got %+v", cfg)
	}
}

func TestLoadEnvOverridesDefault(t *testing.T) {
	t.Setenv(EnvServer, "http://env:8080")
	t.Setenv(EnvAPIKey, "env-key")
	cfg, err := Load(Options{NoConfigFile: true})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server != "http://env:8080" || cfg.APIKey != "env-key" {
		t.Fatalf("got %+v", cfg)
	}
}

func TestLoadFlagOverridesEnv(t *testing.T) {
	t.Setenv(EnvServer, "http://env:8080")
	cfg, err := Load(Options{NoConfigFile: true, ServerFlag: "http://flag:9090", APIKeyFlag: "flag-key"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server != "http://flag:9090" || cfg.APIKey != "flag-key" {
		t.Fatalf("got %+v", cfg)
	}
}

func TestLoadConfigFileWithContext(t *testing.T) {
	isolateEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	writeFile(t, path, `current-context: prod
contexts:
  dev:
    server: http://dev:8080
    api-key: dev-key
  prod:
    server: https://prod.example.com
    api-key: prod-key
`)

	cfg, err := Load(Options{ConfigPath: path})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server != "https://prod.example.com" || cfg.APIKey != "prod-key" {
		t.Fatalf("current-context not applied: %+v", cfg)
	}

	cfg, err = Load(Options{ConfigPath: path, ContextFlag: "dev"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server != "http://dev:8080" || cfg.APIKey != "dev-key" {
		t.Fatalf("--context not applied: %+v", cfg)
	}

	if _, err := Load(Options{ConfigPath: path, ContextFlag: "nope"}); err == nil {
		t.Fatal("expected error for unknown context")
	}
}

func TestLoadConfigFileEnvAndFlagWin(t *testing.T) {
	isolateEnv(t) // clears a possible ambient WEAVECTL_SERVER; sets EnvAPIKey below
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	writeFile(t, path, `current-context: prod
contexts:
  prod:
    server: https://prod.example.com
    api-key: prod-key
`)
	t.Setenv(EnvAPIKey, "env-key")
	cfg, err := Load(Options{ConfigPath: path})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server != "https://prod.example.com" || cfg.APIKey != "env-key" {
		t.Fatalf("env should override file: %+v", cfg)
	}
}

// An explicit --config path that does not exist must be an error, not a
// silent fall back to the default server.
func TestLoadMissingExplicitConfigFileErrors(t *testing.T) {
	isolateEnv(t)
	path := filepath.Join(t.TempDir(), "absent.yaml")
	_, err := Load(Options{ConfigPath: path})
	if err == nil {
		t.Fatal("expected error for explicitly missing config file")
	}
	if !strings.Contains(err.Error(), "absent.yaml") {
		t.Fatalf("error should name the missing path: %v", err)
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("error should wrap os.ErrNotExist: %v", err)
	}
}

// Without an explicit --config, a missing default config file is normal.
func TestLoadMissingDefaultConfigFileIsFine(t *testing.T) {
	isolateEnv(t)
	t.Setenv("HOME", t.TempDir()) // DefaultPath() finds no config here
	cfg, err := Load(Options{})
	if err != nil {
		t.Fatalf("missing default config file must not error: %v", err)
	}
	if cfg.Server != DefaultServer {
		t.Fatalf("got %+v", cfg)
	}
}
