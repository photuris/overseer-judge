// Package config reads overseer-judge's settings from the
// environment and, for the API key, from a key file.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Default settings used when the environment does not override them.
const (
	DefaultBaseURL = "https://api.typesafe.ai"
	DefaultModel   = "jev-latest"
)

// ErrNoKey reports that no API key could be found.
var ErrNoKey = errors.New("no API key")

// Config holds the non-secret settings read from the environment.
type Config struct {
	BaseURL string // TYPESAFE_BASE_URL, default https://api.typesafe.ai
	Model   string // TYPESAFE_DEFAULT_MODEL, default jev-latest
}

// Load reads Config from the environment. It never fails.
func Load() Config {
	cfg := Config{
		BaseURL: os.Getenv("TYPESAFE_BASE_URL"),
		Model:   os.Getenv("TYPESAFE_DEFAULT_MODEL"),
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultBaseURL
	}
	if cfg.Model == "" {
		cfg.Model = DefaultModel
	}

	return cfg
}

// Key returns TYPESAFE_API_KEY if non-empty, else the trimmed
// contents of keyFile. If both are empty or keyFile is unreadable,
// the error wraps ErrNoKey and its message names TYPESAFE_API_KEY and
// keyFile.
func Key(keyFile string) (string, error) {
	if key := strings.TrimSpace(os.Getenv("TYPESAFE_API_KEY")); key != "" {
		return key, nil
	}

	b, err := os.ReadFile(keyFile)
	if err == nil {
		if key := strings.TrimSpace(string(b)); key != "" {
			return key, nil
		}
	}

	return "", fmt.Errorf(
		"%w: set TYPESAFE_API_KEY or write the token to %s",
		ErrNoKey, keyFile,
	)
}

// DefaultKeyFile returns $XDG_CONFIG_HOME/jev, else ~/.config/jev.
func DefaultKeyFile() string {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "jev")
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "jev"
	}

	return filepath.Join(home, ".config", "jev")
}
