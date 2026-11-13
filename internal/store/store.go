// Package store persists the server cache and runtime state to JSON
// files. Writes are atomic (write-temp + rename) so an interrupted run
// never leaves a half-written cache that breaks the next invocation.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/yourgfslove/xray-waybar-ctl/internal/server"
)

// Cache is the on-disk representation of the most recent subscription fetch.
type Cache struct {
	FetchedAt time.Time       `json:"fetched_at"`
	SourceURL string          `json:"source_url"`
	Servers   []server.Server `json:"servers"`
}

// Age returns how long ago the cache was fetched. Used to decide if a
// refresh is needed.
func (c *Cache) Age() time.Duration { return time.Since(c.FetchedAt) }

// TestResult is a single URL-test measurement for one server.
type TestResult struct {
	Name       string        `json:"name"`
	Latency    time.Duration `json:"latency_ns"`
	Alive      bool          `json:"alive"`
	Error      string        `json:"error,omitempty"`
	MeasuredAt time.Time     `json:"measured_at"`
	Source     string        `json:"source,omitempty"` // "url" or "tcp"; empty in legacy state
	URLTest    *ProbeResult  `json:"url_test,omitempty"`
	TCPPing    *ProbeResult  `json:"tcp_ping,omitempty"`
}

// ProbeResult records TCP reachability separately from working proxy access.
// An open TCP port must not turn a failed URL test into a healthy proxy.
type ProbeResult struct {
	Latency    time.Duration `json:"latency_ns"`
	Alive      bool          `json:"alive"`
	Error      string        `json:"error,omitempty"`
	MeasuredAt time.Time     `json:"measured_at"`
}

// State captures runtime information: which server is currently active,
// and the most recent batch of test results.
type State struct {
	ConnectedAt time.Time             `json:"connected_at,omitempty"`
	Active      *server.Server        `json:"active,omitempty"`
	TestedAt    time.Time             `json:"tested_at,omitempty"`
	Results     map[string]TestResult `json:"results,omitempty"`

	// WatchdogAttempts counts consecutive auto-reconnect attempts since
	// the last healthy check. The watchdog pauses (sets WatchdogPausedUntil)
	// once this crosses a threshold so a broken subscription doesn't loop
	// `launch` forever. Reset to 0 on a healthy probe.
	WatchdogAttempts int `json:"watchdog_attempts,omitempty"`

	// WatchdogPausedUntil is set when the watchdog has given up the
	// current burst of reconnect attempts. While time.Now() is before
	// this timestamp, ticks are skipped — but Active is preserved so
	// the next tick after the pause window expires (or the next
	// user-driven connect / post-resume event) picks up where we left
	// off. Reset by successful launch() and by post-resume handler.
	WatchdogPausedUntil time.Time `json:"watchdog_paused_until,omitempty"`

	// Profile overrides cfg.RoutingProfile when non-empty. Set by
	// `xray-waybar-ctl profile <name>`; cleared by `profile reset`.
	Profile string `json:"profile,omitempty"`
}

// LoadCache reads the cache file. A missing file is not an error —
// it returns a zero Cache and (nil) so the caller can decide to fetch.
func LoadCache(path string) (*Cache, error) {
	var c Cache
	if err := readJSON(path, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// SaveCache writes the cache atomically.
func SaveCache(path string, c *Cache) error {
	return writeJSON(path, c)
}

// LoadState reads the runtime state file. Missing file → empty State.
func LoadState(path string) (*State, error) {
	var s State
	if err := readJSON(path, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// SaveState writes runtime state atomically.
func SaveState(path string, s *State) error {
	return writeJSON(path, s)
}

func readJSON(path string, v any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("store: read %s: %w", path, err)
	}
	if len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("store: parse %s: %w", path, err)
	}
	return nil
}

func writeJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("store: mkdir for %s: %w", path, err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return fmt.Errorf("store: temp for %s: %w", path, err)
	}
	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return fmt.Errorf("store: encode %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("store: close %s: %w", path, err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("store: rename %s: %w", path, err)
	}
	return nil
}
