//go:build live

package review

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"overseer-judge/internal/config"
	"overseer-judge/internal/jev"
)

// liveTimeout bounds each live HTTP attempt. A review item is a
// larger state than a pane tail, and a cold model is slower than a
// warm one.
const liveTimeout = 60 * time.Second

// expectation is one item's labels in expect.json.
type expectation struct {
	StyleOnly bool     `json:"style_only"`
	Responses []string `json:"responses"`
}

// TestLiveFixtures types every labelled item against the real API and
// records what came back. This is the POC's measurement, so it runs
// every item even after a failure.
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

	for _, file := range slices.Sorted(maps.Keys(expect)) {
		raw, err := os.ReadFile(filepath.Join("testdata", file))
		if err != nil {
			t.Errorf("%s: read: %v", file, err)

			continue
		}

		for _, it := range Parse(string(raw)) {
			want, ok := expect[file][it.ID]
			if !ok {
				t.Errorf("%s: %s is not in expect.json", file, it.ID)

				continue
			}

			typed, err := Judge(
				context.Background(), client, []Item{it},
			)
			if err != nil {
				t.Errorf("%s %s: Judge: %v", file, it.ID, err)

				continue
			}

			check(t, file, typed[0], want)
		}
	}
}

// check logs one item's judgment and compares it with its labels.
func check(t *testing.T, file string, got Typed, want expectation) {
	t.Helper()

	kinds := make([]string, 0, len(got.Responses))
	for _, r := range got.Responses {
		kinds = append(kinds, fmt.Sprintf(
			"%s(%.2f)", r.Kind, r.Confidence,
		))
	}

	t.Logf("%s %s: style_only=%.2f kinds=%v",
		file, got.ID, got.StyleOnly, kinds)

	if (got.StyleOnly > 0.5) != want.StyleOnly {
		t.Errorf("%s %s: style_only = %.2f, want %v at threshold 0.5",
			file, got.ID, got.StyleOnly, want.StyleOnly)
	}

	if len(got.Responses) != len(want.Responses) {
		t.Errorf("%s %s: %d responses, want %d",
			file, got.ID, len(got.Responses), len(want.Responses))

		return
	}
	for i, r := range got.Responses {
		if r.Kind != want.Responses[i] {
			t.Errorf("%s %s: response %d = %s (%.2f), want %s",
				file, got.ID, i, r.Kind, r.Confidence,
				want.Responses[i])
		}
	}
}

// loadExpectations reads the labels every fixture is judged against.
func loadExpectations(t *testing.T) map[string]map[string]expectation {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join("testdata", "expect.json"))
	if err != nil {
		t.Fatalf("read expect.json: %v", err)
	}

	var expect map[string]map[string]expectation
	if err := json.Unmarshal(raw, &expect); err != nil {
		t.Fatalf("decode expect.json: %v", err)
	}
	if len(expect) == 0 {
		t.Fatal("expect.json lists no fixtures")
	}

	return expect
}
