//go:build live

package tasklint

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/photuris/overseer-judge/internal/config"
	"github.com/photuris/overseer-judge/internal/jev"
)

// liveTimeout bounds each live HTTP attempt. It is longer than the
// CLI default because a task file is a larger state than a pane tail
// and a cold model is slower than a warm one.
const liveTimeout = 60 * time.Second

// TestLiveFixtures lints every labelled fixture against the real API
// and records the judgments it came back with. This is the POC's
// measurement, so it runs every fixture even after a failure.
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

	expect := loadExpectations(t)

	for _, name := range slices.Sorted(maps.Keys(expect)) {
		path := filepath.Join("testdata", name)

		raw, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("%s: read: %v", name, err)

			continue
		}

		report, err := Judge(
			context.Background(), client, path, string(raw),
		)
		if err != nil {
			t.Errorf("%s: Judge: %v", name, err)

			continue
		}

		if report.Acceptance == nil {
			t.Errorf("%s: no acceptance score", name)

			continue
		}

		t.Logf("%s: needs_interpretation=%.2f scope_generic=%.2f "+
			"acceptance=%.2f (conf %.2f)",
			name,
			report.Judgments["needs_interpretation"],
			report.Judgments["scope_generic"],
			report.Acceptance.Score,
			report.Acceptance.Confidence)

		for _, id := range slices.Sorted(maps.Keys(expect[name])) {
			want := expect[name][id]
			if id == "acceptance_sound" {
				score := report.Acceptance.Score
				if (score >= SoundCut) != want {
					t.Errorf("%s: acceptance = %.2f, want sound=%v "+
						"at cut %.2f", name, score, want, SoundCut)
				}

				continue
			}

			p := report.Judgments[id]
			if (p > 0.5) != want {
				t.Errorf("%s: %s = %.2f, want %v at threshold 0.5",
					name, id, p, want)
			}
		}
	}
}

// loadExpectations reads the labels every fixture is judged against.
func loadExpectations(t *testing.T) map[string]map[string]bool {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join("testdata", "expect.json"))
	if err != nil {
		t.Fatalf("read expect.json: %v", err)
	}

	var expect map[string]map[string]bool
	if err := json.Unmarshal(raw, &expect); err != nil {
		t.Fatalf("decode expect.json: %v", err)
	}
	if len(expect) == 0 {
		t.Fatal("expect.json lists no fixtures")
	}

	return expect
}
