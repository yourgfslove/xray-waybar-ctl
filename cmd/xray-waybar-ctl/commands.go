package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/yourgfslove/xray-waybar-ctl/internal/appconfig"
	"github.com/yourgfslove/xray-waybar-ctl/internal/geo"
	"github.com/yourgfslove/xray-waybar-ctl/internal/pinger"
	"github.com/yourgfslove/xray-waybar-ctl/internal/process"
	"github.com/yourgfslove/xray-waybar-ctl/internal/store"
	"github.com/yourgfslove/xray-waybar-ctl/internal/sysmode"
	"github.com/yourgfslove/xray-waybar-ctl/internal/tester"
	"github.com/yourgfslove/xray-waybar-ctl/internal/waybar"
)

// knownProfiles lists profile names the menu and `profile` CLI offer
// out of the box. Users can also pass an arbitrary http(s):// URL to
// `profile <url>` — that is accepted but not listed here.
func knownProfiles() []string {
	out := append([]string{}, appconfig.BuiltinProfiles...)
	for k := range appconfig.RulesPresets {
		out = append(out, k)
	}
	// Stable order so the walker menu doesn't shuffle on every open.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

// menuSep separates fields in the dmenu line. Three U+2002 EN-SPACE
// characters render as a wide gap that is visually obvious yet still
// safe to split on (none of the server names ship with U+2002).
const menuSep = "   "

// cmdStatus emits one line of JSON for waybar. It must be fast — waybar
// polls it on an interval — and must never fail with a non-zero exit
// because waybar would then leave the previous icon stuck on screen.
func cmdStatus(_ context.Context) error {
	lc, err := loadAll()
	if err != nil {
		emitWaybar(waybar.Error(err.Error()))
		return nil
	}

	running, _ := process.IsRunning(lc.cfg.PIDFile)
	if !running || lc.state.Active == nil {
		emitWaybar(waybar.Disconnected())
		return nil
	}
	opt := waybar.ConnectedOptions{
		LocalPort:   lc.cfg.XrayPort,
		ConnectedAt: lc.state.ConnectedAt,
		SystemWide:  lc.cfg.SystemWide,
		Profile:     activeProfile(lc),
	}
	if lc.cfg.SystemWide {
		if state, _ := sysmode.Status(context.Background()); state == sysmode.StateActive {
			opt.TunActive = true
		}
	}
	if r, ok := lc.state.Results[lc.state.Active.Name]; ok && r.Alive {
		opt.Latency = r.Latency
	}
	emitWaybar(waybar.Connected(*lc.state.Active, opt))
	return nil
}

func emitWaybar(s waybar.Status) {
	raw, _ := waybar.Marshal(s)
	os.Stdout.Write(raw)
	os.Stdout.Write([]byte{'\n'})
}

func cmdConnect(ctx context.Context) error {
	// Block until any in-flight watchdog tick releases. Without this,
	// `disconnect` followed by `connect` (or two `connect`s back-to-back)
	// can race a watchdog tick that cached `state.Active` before our
	// disconnect cleared it — leaving a phantom xray the user didn't
	// ask for.
	lock, err := acquireUserLock()
	if err != nil {
		return err
	}
	defer releaseWatchdogLock(lock)

	lc, err := loadAll()
	if err != nil {
		return err
	}
	if err := ensureFreshCache(ctx, lc, false); err != nil {
		return err
	}
	if len(lc.cache.Servers) == 0 {
		return errNoServers
	}

	// If a previous session left the tunnel and xray running, tear
	// them down before testing — otherwise the temp xray instances
	// the tester spawns get their traffic looped back into tun0 and
	// every server appears dead.
	if lc.cfg.SystemWide {
		if err := sysmode.Stop(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "warn: stop tunnel before test: %v\n", err)
		}
	}
	if running, _ := process.IsRunning(lc.cfg.PIDFile); running {
		if err := process.Stop(lc.cfg.PIDFile, 3*time.Second); err != nil {
			fmt.Fprintf(os.Stderr, "warn: stop xray before test: %v\n", err)
		}
	}

	fmt.Fprintf(os.Stderr, "testing %d servers…\n", len(lc.cache.Servers))
	results := tester.Test(ctx, lc.cache.Servers, tester.Options{
		XrayBin:     lc.cfg.XrayBin,
		TestURL:     lc.cfg.TestURL,
		Timeout:     lc.cfg.TestTimeout(),
		Concurrency: lc.cfg.TestConcurrency,
		StartPort:   lc.cfg.XrayPort + 100, // avoid clashing with the live xray
	})
	saveResults(lc, results)

	var pickIdx int = -1
	if i := tester.PickByPriority(results, lc.cfg.Priority); i >= 0 {
		pickIdx = i
	} else {
		tester.SortByLatency(results)
		if len(results) > 0 && results[0].Alive {
			// PickByPriority used the pre-sort index; for the sort
			// fallback we already mutated results, so index 0 is the
			// best.
			pickIdx = 0
		}
	}
	if pickIdx < 0 {
		return fmt.Errorf("no alive servers found")
	}
	picked := results[pickIdx]
	fmt.Fprintf(os.Stderr, "connecting to %s (%dms)\n", picked.Server.DisplayName(), picked.Latency.Milliseconds())
	return launch(ctx, lc, picked.Server, true)
}

func cmdDisconnect(ctx context.Context) error {
	// See cmdConnect for the lock rationale. Critically here: without
	// the lock, a watchdog tick that started a few ms before disconnect
	// will still see `state.Active != nil` in its in-memory snapshot,
	// observe the just-killed xray as unhealthy, and reconnect — making
	// disconnect look broken to the user ("включается обратно").
	lock, err := acquireUserLock()
	if err != nil {
		return err
	}
	defer releaseWatchdogLock(lock)

	lc, err := loadAll()
	if err != nil {
		return err
	}
	// Stop the tunnel *before* killing xray so no packets get sent into
	// a dead SOCKS5 backend.
	if lc.cfg.SystemWide {
		if err := sysmode.Stop(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "warn: system_wide tunnel stop: %v\n", err)
		}
	}
	if err := process.Stop(lc.cfg.PIDFile, 3*time.Second); err != nil {
		return err
	}
	lc.state.Active = nil
	lc.state.ConnectedAt = time.Time{}
	return store.SaveState(lc.cfg.StateFile, lc.state)
}

func cmdToggle(ctx context.Context) error {
	lc, err := loadAll()
	if err != nil {
		return err
	}
	running, _ := process.IsRunning(lc.cfg.PIDFile)
	if running {
		return cmdDisconnect(ctx)
	}
	return cmdConnect(ctx)
}

func cmdReconnect(ctx context.Context) error {
	if err := cmdDisconnect(ctx); err != nil {
		return err
	}
	return cmdConnect(ctx)
}

func cmdUpdate(ctx context.Context) error {
	lc, err := loadAll()
	if err != nil {
		return err
	}
	if err := ensureFreshCache(ctx, lc, true); err != nil {
		return err
	}
	fmt.Printf("cached %d servers from %s\n", len(lc.cache.Servers), lc.cfg.SubscriptionURL)
	return nil
}

func cmdList(_ context.Context) error {
	lc, err := loadAll()
	if err != nil {
		return err
	}
	if len(lc.cache.Servers) == 0 {
		return errNoServers
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "#\tNAME\tPROTOCOL\tADDR\tSECURITY\tLATENCY\tSTATUS")
	for i, s := range lc.cache.Servers {
		latency := "—"
		status := ""
		if r, ok := lc.state.Results[s.Name]; ok {
			if r.Alive {
				latency = fmt.Sprintf("%dms", r.Latency.Milliseconds())
			} else {
				latency = "dead"
			}
		}
		if lc.state.Active != nil && lc.state.Active.Name == s.Name {
			status = "*active"
		}
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\t%s\n",
			i, s.DisplayName(), s.Protocol, s.Endpoint(), s.Security, latency, status)
	}
	return w.Flush()
}

func cmdTest(ctx context.Context) error {
	// Testing temporarily changes TUN state. Keep watchdog and user-driven
	// connection changes from racing the test and its saved results.
	lock, err := acquireUserLock()
	if err != nil {
		return err
	}
	defer releaseWatchdogLock(lock)

	lc, err := loadAll()
	if err != nil {
		return err
	}
	if err := ensureFreshCache(ctx, lc, false); err != nil {
		return err
	}
	if len(lc.cache.Servers) == 0 {
		return errNoServers
	}
	// Tear down TUN before testing: the test xray instances dial
	// upstream servers as plain net traffic from this process. With
	// TUN up and SOMark on the main xray only, the test connections
	// fall through to tun0 → tun2socks → socks-in → main xray → ...
	// which floods ephemeral ports and falsifies the measurement
	// (we'd be timing main xray's proxy outbound, not the test
	// server). Same precaution cmdConnect takes.
	tunWasUp := false
	if lc.cfg.SystemWide {
		if state, _ := sysmode.Status(ctx); state == sysmode.StateActive {
			tunWasUp = true
		}
		if err := sysmode.Stop(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "warn: stop tunnel before test: %v\n", err)
		}
	}
	results := tester.Test(ctx, lc.cache.Servers, tester.Options{
		XrayBin:     lc.cfg.XrayBin,
		TestURL:     lc.cfg.TestURL,
		Timeout:     lc.cfg.TestTimeout(),
		Concurrency: lc.cfg.TestConcurrency,
		StartPort:   lc.cfg.XrayPort + 100,
	})
	if tunWasUp {
		if err := sysmode.Start(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "warn: restart tunnel after test: %v\n", err)
		}
	}
	saveResults(lc, results)

	tester.SortByLatency(results)
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tADDR\tLATENCY\tERROR")
	for _, r := range results {
		lat := "dead"
		errStr := ""
		if r.Alive {
			lat = fmt.Sprintf("%dms", r.Latency.Milliseconds())
		} else if r.Error != nil {
			errStr = r.Error.Error()
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
			r.Server.DisplayName(), r.Server.Endpoint(), lat, errStr)
	}
	return w.Flush()
}

func cmdUse(ctx context.Context, name string) error {
	lock, err := acquireUserLock()
	if err != nil {
		return err
	}
	defer releaseWatchdogLock(lock)

	lc, err := loadAll()
	if err != nil {
		return err
	}
	if err := ensureFreshCache(ctx, lc, false); err != nil {
		return err
	}
	s, _, err := findServer(lc.cache, name)
	if err != nil {
		return err
	}
	return launch(ctx, lc, s, true)
}

// cmdUseDir picks the next/previous server relative to the active one.
// If there is no active server, starts from the head of the list.
func cmdUseDir(ctx context.Context, step int) error {
	lock, err := acquireUserLock()
	if err != nil {
		return err
	}
	defer releaseWatchdogLock(lock)

	lc, err := loadAll()
	if err != nil {
		return err
	}
	if err := ensureFreshCache(ctx, lc, false); err != nil {
		return err
	}
	if len(lc.cache.Servers) == 0 {
		return errNoServers
	}
	idx := activeIndex(lc)
	if idx < 0 {
		idx = 0
	} else {
		idx = (idx + step + len(lc.cache.Servers)) % len(lc.cache.Servers)
	}
	return launch(ctx, lc, lc.cache.Servers[idx], true)
}

// cmdPing runs a TCP-level liveness check on every cached server and
// merges the result into state.Results. It is the cheap, periodic
// counterpart to `test` — meant to be driven by a 30s systemd timer.
//
// TCP reachability is saved separately once a URL test exists. Until then,
// including for legacy state without a source, TCP provides a provisional
// result. A failed TCP ping can mark a proxy unreachable; a successful one
// cannot erase a failed URL test or substitute its latency for URL latency.
func cmdPing(ctx context.Context) error {
	lock, err := acquireUserLock()
	if err != nil {
		return err
	}
	defer releaseWatchdogLock(lock)

	lc, err := loadAll()
	if err != nil {
		return err
	}
	if len(lc.cache.Servers) == 0 {
		return errNoServers
	}

	results := pinger.PingAll(ctx, lc.cache.Servers, lc.cfg.TestTimeout())
	if lc.state.Results == nil {
		lc.state.Results = make(map[string]store.TestResult, len(results))
	}
	now := time.Now()
	for _, r := range results {
		lc.state.Results[r.Server.Name] = mergePingResult(lc.state.Results[r.Server.Name], r, now)
	}
	return store.SaveState(lc.cfg.StateFile, lc.state)
}

func mergePingResult(prev store.TestResult, r pinger.Result, now time.Time) store.TestResult {
	ping := &store.ProbeResult{Latency: r.Latency, Alive: r.Alive, MeasuredAt: now}
	if r.Error != nil {
		ping.Error = r.Error.Error()
	}
	prev.TCPPing = ping
	prev.Name = r.Server.Name
	result := ping
	prev.Source = "tcp"
	if prev.URLTest != nil && (ping.Alive || !prev.URLTest.Alive) {
		result = prev.URLTest
		prev.Source = "url"
	}
	prev.Latency = result.Latency
	prev.Alive = result.Alive
	prev.Error = result.Error
	prev.MeasuredAt = result.MeasuredAt
	return prev
}

// cmdMenu opens a walker --dmenu picker of cached servers. The first
// row is a "go to profiles" pivot — selecting it opens a second walker
// listing the routing profiles, so the main list stays focused on
// servers.
//
// We don't kick off a fresh URL-test here on purpose — that takes
// several seconds and the user clicked expecting an immediate picker.
// Latency comes from the last `test`/`connect` batch via state.json.
// If the user wants a fresh measurement, they can run `test` first
// or scroll on the waybar pill to bisect manually.
func cmdMenu(ctx context.Context) error {
	lc, err := loadAll()
	if err != nil {
		return err
	}
	if err := ensureFreshCache(ctx, lc, false); err != nil {
		return err
	}
	if len(lc.cache.Servers) == 0 {
		return errNoServers
	}

	var lines strings.Builder
	currentName := ""
	if lc.state.Active != nil {
		currentName = lc.state.Active.Name
	}
	// One pivot row. The leading gear glyph + trailing arrow telegraph
	// "this opens another menu", and is also how parseMenuChoice routes
	// the selection to cmdMenuProfiles.
	fmt.Fprintf(&lines, "%s%sProfile: %s%s→\n", profileMenuMarker, menuSep, activeProfile(lc), menuSep)
	currentIdx := -1
	for i, s := range lc.cache.Servers {
		if s.Name == currentName {
			// +1 because the pivot row is line 0.
			currentIdx = i + 1
			break
		}
	}
	for _, s := range lc.cache.Servers {
		marker := "○" // untested
		info := "—"
		if r, ok := lc.state.Results[s.Name]; ok {
			if r.Alive {
				marker = "●"
				info = fmt.Sprintf("%dms", r.Latency.Milliseconds())
			} else {
				marker = "✗"
				info = "dead"
			}
		}
		prefix := "  "
		if s.Name == currentName {
			prefix = "→ "
		}
		fmt.Fprintf(&lines, "%s%s%s%s%s%s\n", prefix, marker, menuSep, s.Name, menuSep, info)
	}

	choice, err := runWalker(ctx, lines.String(), "Pick a server", currentIdx)
	if err != nil || choice == "" {
		return err
	}
	if strings.HasPrefix(choice, profileMenuMarker) {
		// Fork a fresh `xray-waybar-ctl menu-profiles` instead of
		// calling cmdMenuProfiles inline. Two walker invocations
		// back-to-back in the same parent process race on focus and
		// keyboard input under Hyprland — the Enter that closed the
		// first walker is replayed into the just-spawned second one,
		// which silently picks the first row and exits. A detached
		// child started after the first walker fully released its
		// Wayland surface gets a clean activation. Fire-and-forget:
		// the user already moved on from the server menu, no need
		// to wait or report errors back here.
		return spawnDetached("menu-profiles")
	}
	parts := strings.Split(choice, menuSep)
	if len(parts) < 2 {
		return fmt.Errorf("unparseable menu selection %q", choice)
	}
	name := strings.TrimSpace(parts[1])
	return cmdUse(ctx, name)
}

// cmdMenuProfiles is the second-level walker showing every known
// profile (built-ins + RulesPresets). Selecting one delegates to
// cmdProfile, which handles the download + reconnect.
func cmdMenuProfiles(ctx context.Context) error {
	lc, err := loadAll()
	if err != nil {
		return err
	}
	current := activeProfile(lc)
	var lines strings.Builder
	currentIdx := -1
	for i, p := range knownProfiles() {
		tag := ""
		if p == current {
			tag = "(active)"
			currentIdx = i
		}
		// Same leading glyph as the pivot row so the menus feel like
		// one continuous walker session.
		fmt.Fprintf(&lines, "%s%s%s%s%s\n", profileMenuMarker, menuSep, p, menuSep, tag)
	}
	choice, err := runWalker(ctx, lines.String(), "Routing profile", currentIdx)
	if err != nil || choice == "" {
		return err
	}
	parts := strings.Split(choice, menuSep)
	if len(parts) < 2 {
		return fmt.Errorf("unparseable profile selection %q", choice)
	}
	name := strings.TrimSpace(parts[1])
	return cmdProfile(ctx, []string{name})
}

// runWalker is the small wrapper around `walker --dmenu` shared by the
// server and profile menus. Esc / no selection produces empty stdout
// and we return ("", nil); a real spawn failure (walker not on PATH,
// killed by ctx) now surfaces instead of being swallowed as Esc — that
// previously hid the very bug this comment is about.
//
// currentIdx is the 0-based line index to preselect, or -1 for none.
// Walker's --current takes an integer, NOT a string match — passing a
// name like "smart" makes walker exit with `Cannot parse integer value`
// and, in a detached child whose stderr is /dev/null, the menu just
// silently never appears.
func runWalker(ctx context.Context, stdin, placeholder string, currentIdx int) (string, error) {
	args := []string{"--dmenu", "--placeholder", placeholder}
	if currentIdx >= 0 {
		args = append(args, "--current", strconv.Itoa(currentIdx))
	}
	cmd := exec.CommandContext(ctx, "walker", args...)
	cmd.Stdin = strings.NewReader(stdin)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		if _, ok := err.(*exec.ExitError); ok {
			// Non-zero exit: walker convention for "user dismissed".
			return "", nil
		}
		return "", fmt.Errorf("walker: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// spawnDetached re-execs this binary with the given subcommand as a
// fully detached child (new session, stdio redirected to /dev/null) so
// no Wayland focus / keyboard state leaks from the parent into the
// child's walker invocation. Returns once the child has been started
// — we deliberately do not wait for it.
func spawnDetached(subcommand string) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("spawnDetached: locate self: %w", err)
	}
	devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("spawnDetached: open /dev/null: %w", err)
	}
	defer devnull.Close()

	cmd := exec.Command(self, subcommand)
	cmd.Stdin = devnull
	cmd.Stdout = devnull
	cmd.Stderr = devnull
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("spawnDetached: start %s: %w", subcommand, err)
	}
	// Release so the child doesn't become a zombie if our caller is
	// short-lived (it always is — cmdMenu returns right after this).
	return cmd.Process.Release()
}

// profileMenuMarker is the leading glyph the menu parser uses to tell
// "open profile picker" from a server pick. Picked to be visually
// distinct and to never appear in a server fragment.
const profileMenuMarker = "⚙"

// cmdUpdateGeo downloads geoip.dat / geosite.dat plus the rules.json
// for the active profile (if it has one). When any file actually
// changed and xray is currently running on a rules-backed profile,
// triggers a reconnect so the new lists take effect.
func cmdUpdateGeo(ctx context.Context) error {
	lc, err := loadAll()
	if err != nil {
		return err
	}
	rulesURL := ""
	if profileNeedsRules(activeProfile(lc)) {
		rulesURL = appconfig.ProfileRulesURL(activeProfile(lc))
	}
	results, ferr := geo.Update(ctx, geo.Sources{
		Dir:        lc.cfg.Geo.Dir,
		GeoipURL:   lc.cfg.Geo.GeoipURL,
		GeositeURL: lc.cfg.Geo.GeositeURL,
		RulesURL:   rulesURL,
	})
	for _, r := range results {
		state := "unchanged"
		if r.Changed {
			state = "updated"
		}
		fmt.Fprintf(os.Stderr, "%s: %s (%d bytes)\n", r.Name, state, r.Bytes)
	}
	if ferr != nil {
		return ferr
	}

	anyChanged := false
	for _, r := range results {
		if r.Changed {
			anyChanged = true
			break
		}
	}
	if !anyChanged {
		return nil
	}
	if running, _ := process.IsRunning(lc.cfg.PIDFile); running && profileNeedsRules(activeProfile(lc)) {
		// Don't bounce a live tunnel just to load new geo lists. xray
		// only reads geoip.dat/geosite.dat at startup, so picking them up
		// means a stop/start of xray + a TUN restart — every daily timer
		// tick (and the OnBootSec one ~2min after login) would drop tun0
		// and kill all routed traffic for a few seconds. The refreshed
		// lists aren't time-critical; let the next natural reconnect
		// (server switch, resume, or manual `reconnect`) apply them.
		fmt.Fprintln(os.Stderr, "geo assets changed — will apply on next reconnect")
	}
	return nil
}

// cmdProfile prints the active profile when called without args, or
// updates state.Profile and reconnects when called with one. Accepts:
//   - "proxy-all" / "direct"                       — built-ins
//   - any key from appconfig.RulesPresets          — downloads its rules
//   - a raw http(s):// URL to a HAPP rules JSON    — downloads it
//   - "reset"                                      — clears the override
func cmdProfile(ctx context.Context, args []string) error {
	lc, err := loadAll()
	if err != nil {
		return err
	}
	if len(args) == 0 {
		current := activeProfile(lc)
		source := "config"
		if lc.state.Profile != "" {
			source = "state override"
		}
		fmt.Printf("current profile: %s (%s)\n", current, source)
		fmt.Printf("known: %s\n", strings.Join(knownProfiles(), ", "))
		fmt.Println("also accepted: any http(s):// URL to a HAPP rules JSON, or 'reset'")
		return nil
	}
	name := args[0]
	if name == "reset" {
		lc.state.Profile = ""
	} else {
		if err := appconfig.ValidateProfile(name); err != nil {
			return err
		}
		lc.state.Profile = name
	}
	if err := store.SaveState(lc.cfg.StateFile, lc.state); err != nil {
		return err
	}
	resolved := activeProfile(lc)
	fmt.Fprintf(os.Stderr, "profile set to %s\n", resolved)

	// If the new profile needs rules.json, fetch it now. We call
	// geo.Update directly (rather than cmdUpdateGeo) so we don't end
	// up reconnecting twice — the explicit cmdReconnect below is the
	// canonical point for that.
	if profileNeedsRules(resolved) {
		results, ferr := geo.Update(ctx, geo.Sources{
			Dir:        lc.cfg.Geo.Dir,
			GeoipURL:   lc.cfg.Geo.GeoipURL,
			GeositeURL: lc.cfg.Geo.GeositeURL,
			RulesURL:   appconfig.ProfileRulesURL(resolved),
		})
		for _, r := range results {
			state := "unchanged"
			if r.Changed {
				state = "updated"
			}
			fmt.Fprintf(os.Stderr, "%s: %s (%d bytes)\n", r.Name, state, r.Bytes)
		}
		if ferr != nil {
			fmt.Fprintf(os.Stderr, "warn: update-geo for profile %q: %v\n", resolved, ferr)
		}
	} else {
		// Built-in profile — drop the stale rules.json so a fallback
		// reload doesn't accidentally pick up the previous profile's
		// rules. Keep geoip/geosite (cheap to leave around, harmless).
		_ = geo.RemoveRules(lc.cfg.Geo.Dir)
	}

	// Reconnect only if xray is currently running. Otherwise the next
	// `connect` will pick up the new profile automatically.
	if running, _ := process.IsRunning(lc.cfg.PIDFile); !running || lc.state.Active == nil {
		return nil
	}
	return cmdReconnect(ctx)
}

func saveResults(lc *loadCtx, results []tester.Result) {
	if lc.state.Results == nil {
		lc.state.Results = make(map[string]store.TestResult, len(results))
	}
	now := time.Now()
	for _, r := range results {
		tr := store.TestResult{
			Name:       r.Server.Name,
			Latency:    r.Latency,
			Alive:      r.Alive,
			MeasuredAt: now,
			Source:     "url",
			TCPPing:    lc.state.Results[r.Server.Name].TCPPing,
		}
		if r.Error != nil {
			tr.Error = r.Error.Error()
		}
		tr.URLTest = &store.ProbeResult{
			Latency: tr.Latency, Alive: tr.Alive, Error: tr.Error, MeasuredAt: tr.MeasuredAt,
		}
		lc.state.Results[r.Server.Name] = tr
	}
	lc.state.TestedAt = now
	_ = store.SaveState(lc.cfg.StateFile, lc.state)
}
