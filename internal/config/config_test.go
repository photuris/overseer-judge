package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoad(t *testing.T) {
	tests := []struct {
		name        string
		baseURL     string
		model       string
		wantBaseURL string
		wantModel   string
	}{
		{
			name:        "defaults",
			wantBaseURL: DefaultBaseURL,
			wantModel:   DefaultModel,
		},
		{
			name:        "overrides",
			baseURL:     "http://127.0.0.1:9",
			model:       "jev-test",
			wantBaseURL: "http://127.0.0.1:9",
			wantModel:   "jev-test",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("TYPESAFE_BASE_URL", tt.baseURL)
			t.Setenv("TYPESAFE_DEFAULT_MODEL", tt.model)

			got := Load()
			if got.BaseURL != tt.wantBaseURL {
				t.Errorf(
					"BaseURL = %q, want %q", got.BaseURL, tt.wantBaseURL,
				)
			}
			if got.Model != tt.wantModel {
				t.Errorf("Model = %q, want %q", got.Model, tt.wantModel)
			}
		})
	}
}

// writeKey writes contents to a file in a temp dir and returns its
// path.
func writeKey(t *testing.T, contents string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "jev")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write key file: %v", err)
	}

	return path
}

func TestKey(t *testing.T) {
	t.Run("env wins over file", func(t *testing.T) {
		t.Setenv("TYPESAFE_API_KEY", "from-env")

		got, err := Key(writeKey(t, "from-file"))
		if err != nil {
			t.Fatalf("Key: %v", err)
		}
		if got != "from-env" {
			t.Errorf("Key = %q, want %q", got, "from-env")
		}
	})

	t.Run("file used when env empty", func(t *testing.T) {
		t.Setenv("TYPESAFE_API_KEY", "")

		got, err := Key(writeKey(t, "  from-file\n"))
		if err != nil {
			t.Fatalf("Key: %v", err)
		}
		if got != "from-file" {
			t.Errorf("Key = %q, want %q", got, "from-file")
		}
	})

	t.Run("whitespace-only file counts as empty", func(t *testing.T) {
		t.Setenv("TYPESAFE_API_KEY", "")

		path := writeKey(t, " \n\t\n")
		if _, err := Key(path); !errors.Is(err, ErrNoKey) {
			t.Fatalf("Key error = %v, want ErrNoKey", err)
		}
	})

	t.Run("both missing", func(t *testing.T) {
		t.Setenv("TYPESAFE_API_KEY", "")

		path := filepath.Join(t.TempDir(), "absent")
		_, err := Key(path)
		if !errors.Is(err, ErrNoKey) {
			t.Fatalf("Key error = %v, want ErrNoKey", err)
		}
		if !strings.Contains(err.Error(), "TYPESAFE_API_KEY") {
			t.Errorf("error %q lacks TYPESAFE_API_KEY", err)
		}
		if !strings.Contains(err.Error(), path) {
			t.Errorf("error %q lacks %q", err, path)
		}
	})
}

func TestDefaultKeyFile(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/xdg")

	if got := DefaultKeyFile(); got != "/xdg/jev" {
		t.Errorf("DefaultKeyFile = %q, want /xdg/jev", got)
	}

	t.Setenv("XDG_CONFIG_HOME", "")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home dir: %v", err)
	}
	want := filepath.Join(home, ".config", "jev")
	if got := DefaultKeyFile(); got != want {
		t.Errorf("DefaultKeyFile = %q, want %q", got, want)
	}
}
