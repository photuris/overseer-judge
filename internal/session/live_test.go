//go:build live

package session

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"overseer-judge/internal/config"
	"overseer-judge/internal/jev"
)

// liveTimeout bounds each live HTTP attempt. It is longer than the
// CLI default because a cold model is slower than a warm one.
const liveTimeout = 30 * time.Second

// TestLiveFixtures classifies every fixture against the real API and
// records the label and confidence it came back with. This is the
// POC's measurement, so it runs every fixture even after a failure.
func TestLiveFixtures(t *testing.T) {
	key, err := config.Key(config.DefaultKeyFile())
	if errors.Is(err, config.ErrNoKey) {
		t.Skip("no API key: set TYPESAFE_API_KEY or write the " +
			"token to " + config.DefaultKeyFile())
	}
	if err != nil {
		t.Fatalf("read API key: %v", err)
	}

	cfg := config.Load()
	client := jev.New(
		&http.Client{Timeout: liveTimeout},
		cfg.BaseURL, key, cfg.Model,
	)

	paths, err := filepath.Glob(filepath.Join("testdata", "*.txt"))
	if err != nil {
		t.Fatalf("list fixtures: %v", err)
	}
	if len(paths) == 0 {
		t.Fatal("no fixtures in testdata")
	}
	slices.Sort(paths)

	for _, path := range paths {
		name := filepath.Base(path)

		raw, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("%s: read: %v", name, err)

			continue
		}

		v, err := Judge(
			context.Background(), client, "unknown", string(raw),
		)
		if err != nil {
			t.Errorf("%s: Judge: %v", name, err)

			continue
		}

		t.Logf("%s: state=%s conf=%.2f coherent=%.2f",
			name, v.State, v.Confidence, v.Coherent)

		if want := expectedLabel(name); v.State != want {
			t.Errorf("%s: state = %q, want %q (probabilities %v)",
				name, v.State, want, v.Probabilities)
		}
	}
}

// expectedLabel returns the label a fixture's filename declares.
func expectedLabel(name string) string {
	label, _, _ := strings.Cut(name, "-")

	return label
}
