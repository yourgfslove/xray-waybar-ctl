package main

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/yourgfslove/xray-waybar-ctl/internal/appconfig"
	"github.com/yourgfslove/xray-waybar-ctl/internal/pinger"
	"github.com/yourgfslove/xray-waybar-ctl/internal/server"
	"github.com/yourgfslove/xray-waybar-ctl/internal/store"
	"github.com/yourgfslove/xray-waybar-ctl/internal/tester"
)

func TestPingDoesNotReviveFailedURLTest(t *testing.T) {
	lc := &loadCtx{
		cfg:   &appconfig.Config{StateFile: filepath.Join(t.TempDir(), "state.json")},
		state: &store.State{Results: make(map[string]store.TestResult)},
	}
	s := server.Server{Name: "XHTTP"}
	saveResults(lc, []tester.Result{{Server: s, Error: context.DeadlineExceeded}})
	// Reload from disk just as the independent timer invocation does.
	state, err := store.LoadState(lc.cfg.StateFile)
	if err != nil {
		t.Fatal(err)
	}
	before := state.Results[s.Name]
	now := before.MeasuredAt.Add(30 * time.Second)
	got := mergePingResult(before, pinger.Result{Server: s, Alive: true, Latency: 50 * time.Millisecond}, now)
	if got.Alive || got.Error != context.DeadlineExceeded.Error() || got.Source != "url" {
		t.Fatalf("TCP ping erased the failed proxy test: %+v", got)
	}
	if got.Latency != before.Latency || !got.MeasuredAt.Equal(before.MeasuredAt) {
		t.Fatal("TCP ping changed the URL measurement")
	}
	if got.TCPPing == nil || !got.TCPPing.Alive || got.TCPPing.Latency != 50*time.Millisecond || !got.TCPPing.MeasuredAt.Equal(now) {
		t.Fatalf("TCP measurement was not recorded separately: %+v", got.TCPPing)
	}
	// A later successful full test is allowed to restore proxy health.
	lc.state = state
	lc.state.Results[s.Name] = got
	saveResults(lc, []tester.Result{{Server: s, Alive: true, Latency: 150 * time.Millisecond}})
	if result := lc.state.Results[s.Name]; !result.Alive || result.Error != "" || result.URLTest == nil || !result.URLTest.Alive {
		t.Fatalf("successful URL retest did not restore health: %+v", result)
	}
}

func TestPingTracksTCPFailureAndRecoveryForWorkingProxy(t *testing.T) {
	now := time.Now()
	s := server.Server{Name: "working"}
	prev := store.TestResult{URLTest: &store.ProbeResult{
		Alive: true, Latency: 200 * time.Millisecond, MeasuredAt: now,
	}}
	got := mergePingResult(prev, pinger.Result{Server: s, Error: errors.New("connection refused")}, now.Add(time.Second))
	if got.Alive || got.Source != "tcp" || got.Error != "connection refused" {
		t.Fatalf("TCP failure did not mark server unreachable: %+v", got)
	}
	got = mergePingResult(got, pinger.Result{Server: s, Alive: true, Latency: 40 * time.Millisecond}, now.Add(2*time.Second))
	if !got.Alive || got.Source != "url" || got.Latency != 200*time.Millisecond || got.Error != "" || !got.MeasuredAt.Equal(now) {
		t.Fatalf("TCP recovery did not restore the existing URL result: %+v", got)
	}
}

func TestPingWithoutURLTestUsesTCPMeasurement(t *testing.T) {
	for _, prev := range []store.TestResult{{}, {Alive: false, Error: "legacy result"}} {
		now := time.Now()
		got := mergePingResult(prev, pinger.Result{
			Server: server.Server{Name: "new"}, Alive: true, Latency: 50 * time.Millisecond,
		}, now)
		if !got.Alive || got.Source != "tcp" || got.Latency != 50*time.Millisecond || got.Error != "" || !got.MeasuredAt.Equal(now) || got.URLTest != nil {
			t.Fatalf("incorrect provisional TCP result: %+v", got)
		}
	}
}
