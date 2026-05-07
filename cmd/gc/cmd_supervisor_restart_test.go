package main

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
)

// --- protocol parsing -------------------------------------------------------

func TestParseRestartCityCommandValidLines(t *testing.T) {
	cases := []struct {
		line     string
		wantName string
		wantForce bool
		wantTO   time.Duration
	}{
		{"restart-city trader force=false timeout_ms=300000", "trader", false, 300 * time.Second},
		{"restart-city jarvis force=true timeout_ms=0", "jarvis", true, 0},
		{"restart-city command-center force=false timeout_ms=60000", "command-center", false, 60 * time.Second},
	}
	for _, tc := range cases {
		name, force, to, err := parseRestartCityCommand(tc.line)
		if err != nil {
			t.Fatalf("parseRestartCityCommand(%q) error = %v, want nil", tc.line, err)
		}
		if name != tc.wantName {
			t.Fatalf("parseRestartCityCommand(%q) name = %q, want %q", tc.line, name, tc.wantName)
		}
		if force != tc.wantForce {
			t.Fatalf("parseRestartCityCommand(%q) force = %v, want %v", tc.line, force, tc.wantForce)
		}
		if to != tc.wantTO {
			t.Fatalf("parseRestartCityCommand(%q) timeout = %s, want %s", tc.line, to, tc.wantTO)
		}
	}
}

func TestParseRestartCityCommandRejectsMalformed(t *testing.T) {
	cases := []string{
		"restart-city",                                       // missing name
		"restart-city  force=true timeout_ms=0",              // empty name
		"restart-city name force=maybe timeout_ms=0",         // bad bool
		"restart-city name force=true timeout_ms=notanumber", // bad timeout
	}
	for _, line := range cases {
		if _, _, _, err := parseRestartCityCommand(line); err == nil {
			t.Fatalf("parseRestartCityCommand(%q) error = nil, want non-nil", line)
		}
	}
}

func TestFormatRestartCityReplyRoundTrips(t *testing.T) {
	line := formatRestartCityReply("aaaaaaaaaaaa", "bbbbbbbbbbbb", 1234, true)
	old, new, drainMs, forced, err := parseRestartCityReply(line)
	if err != nil {
		t.Fatalf("parseRestartCityReply(%q) error = %v", line, err)
	}
	if old != "aaaaaaaaaaaa" || new != "bbbbbbbbbbbb" || drainMs != 1234 || !forced {
		t.Fatalf("round-trip mismatch: got old=%q new=%q drain=%d forced=%v", old, new, drainMs, forced)
	}
}

// --- cobra command ----------------------------------------------------------

func TestSupervisorRestartCityCobraRequiresName(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cmd := newSupervisorRestartCityCmd(&stdout, &stderr)
	cmd.SetArgs([]string{})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	if err := cmd.Execute(); err == nil {
		t.Fatalf("Execute() with no args should fail")
	}
}

func TestSupervisorRestartCityCobraExposesFlags(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cmd := newSupervisorRestartCityCmd(&stdout, &stderr)
	if cmd.Flags().Lookup("force") == nil {
		t.Fatalf("missing --force flag")
	}
	if cmd.Flags().Lookup("timeout") == nil {
		t.Fatalf("missing --timeout flag")
	}
}

// --- client round-trip ------------------------------------------------------

func TestRestartCitySupervisorRoundTripOK(t *testing.T) {
	gcHome := shortTempDir(t, "gc-home-")
	runtimeDir := shortTempDir(t, "gc-run-")
	t.Setenv("GC_HOME", gcHome)
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)

	sockPath := filepath.Join(gcHome, "supervisor.sock")
	var got struct {
		mu      sync.Mutex
		command string
	}
	startTestSupervisorSocket(t, sockPath, func(cmd string) string {
		switch {
		case cmd == "ping":
			return "4242\n"
		case strings.HasPrefix(cmd, "restart-city "):
			got.mu.Lock()
			got.command = cmd
			got.mu.Unlock()
			return formatRestartCityReply("oldsha123456", "newsha654321", 1500, false) + "\n"
		}
		return ""
	})

	var stdout, stderr bytes.Buffer
	if code := restartCitySupervisor(&stdout, &stderr, "trader", false, 5*time.Minute); code != 0 {
		t.Fatalf("restartCitySupervisor code = %d, want 0; stderr=%q", code, stderr.String())
	}

	got.mu.Lock()
	cmd := got.command
	got.mu.Unlock()
	wantPrefix := "restart-city trader force=false timeout_ms=" + strconv.FormatInt((5*time.Minute).Milliseconds(), 10)
	if cmd != wantPrefix {
		t.Fatalf("server received %q, want %q", cmd, wantPrefix)
	}
	if !strings.Contains(stdout.String(), "trader") {
		t.Fatalf("stdout = %q, want city name", stdout.String())
	}
	if !strings.Contains(stdout.String(), "oldsha") || !strings.Contains(stdout.String(), "newsha") {
		t.Fatalf("stdout = %q, want both binary SHAs", stdout.String())
	}
}

func TestRestartCitySupervisorRoundTripForce(t *testing.T) {
	gcHome := shortTempDir(t, "gc-home-")
	runtimeDir := shortTempDir(t, "gc-run-")
	t.Setenv("GC_HOME", gcHome)
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)

	sockPath := filepath.Join(gcHome, "supervisor.sock")
	var got struct {
		mu      sync.Mutex
		command string
	}
	startTestSupervisorSocket(t, sockPath, func(cmd string) string {
		switch {
		case cmd == "ping":
			return "4242\n"
		case strings.HasPrefix(cmd, "restart-city "):
			got.mu.Lock()
			got.command = cmd
			got.mu.Unlock()
			return formatRestartCityReply("oldsha", "newsha", 0, true) + "\n"
		}
		return ""
	})

	var stdout, stderr bytes.Buffer
	if code := restartCitySupervisor(&stdout, &stderr, "trader", true, 1*time.Minute); code != 0 {
		t.Fatalf("restartCitySupervisor code = %d, want 0; stderr=%q", code, stderr.String())
	}

	got.mu.Lock()
	cmd := got.command
	got.mu.Unlock()
	if !strings.Contains(cmd, "force=true") {
		t.Fatalf("server received %q, want force=true", cmd)
	}
	if !strings.Contains(stdout.String(), "forced") {
		t.Fatalf("stdout = %q, want 'forced' indicator", stdout.String())
	}
}

func TestRestartCitySupervisorNotFound(t *testing.T) {
	gcHome := shortTempDir(t, "gc-home-")
	runtimeDir := shortTempDir(t, "gc-run-")
	t.Setenv("GC_HOME", gcHome)
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)

	sockPath := filepath.Join(gcHome, "supervisor.sock")
	startTestSupervisorSocket(t, sockPath, func(cmd string) string {
		switch {
		case cmd == "ping":
			return "4242\n"
		case strings.HasPrefix(cmd, "restart-city "):
			return "error not_found: city \"phantom\" not registered\n"
		}
		return ""
	})

	var stdout, stderr bytes.Buffer
	code := restartCitySupervisor(&stdout, &stderr, "phantom", false, time.Minute)
	if code != 1 {
		t.Fatalf("restartCitySupervisor code = %d, want 1 for not_found", code)
	}
	if !strings.Contains(stderr.String(), "not_found") && !strings.Contains(stderr.String(), "phantom") {
		t.Fatalf("stderr = %q, want not_found details", stderr.String())
	}
}

func TestRestartCitySupervisorBusy(t *testing.T) {
	gcHome := shortTempDir(t, "gc-home-")
	runtimeDir := shortTempDir(t, "gc-run-")
	t.Setenv("GC_HOME", gcHome)
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)

	sockPath := filepath.Join(gcHome, "supervisor.sock")
	startTestSupervisorSocket(t, sockPath, func(cmd string) string {
		switch {
		case cmd == "ping":
			return "4242\n"
		case strings.HasPrefix(cmd, "restart-city "):
			return "busy\n"
		}
		return ""
	})

	var stdout, stderr bytes.Buffer
	code := restartCitySupervisor(&stdout, &stderr, "trader", false, time.Minute)
	if code != 1 {
		t.Fatalf("restartCitySupervisor code = %d, want 1 for busy", code)
	}
	if !strings.Contains(stderr.String(), "busy") {
		t.Fatalf("stderr = %q, want busy hint", stderr.String())
	}
}

func TestRestartCitySupervisorWhenSupervisorNotRunning(t *testing.T) {
	gcHome := shortTempDir(t, "gc-home-")
	runtimeDir := shortTempDir(t, "gc-run-")
	t.Setenv("GC_HOME", gcHome)
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)

	var stdout, stderr bytes.Buffer
	code := restartCitySupervisor(&stdout, &stderr, "trader", false, time.Minute)
	if code != 1 {
		t.Fatalf("restartCitySupervisor code = %d, want 1 when supervisor not running", code)
	}
	if !strings.Contains(stderr.String(), "supervisor is not running") {
		t.Fatalf("stderr = %q, want 'supervisor is not running'", stderr.String())
	}
}

// --- core restart engine ----------------------------------------------------

// fakeManagedCity builds a managedCity stub suitable for the
// performCityRestart tests. drain is closed when cancel runs (so the
// stop wait observes a clean drain), unless skipAutoDrain is true (in
// which case the test must trigger drain manually to exercise timeout
// or force paths).
func fakeManagedCity(t *testing.T, name string, skipAutoDrain bool) (*managedCity, chan struct{}, *atomic.Int32) {
	t.Helper()
	done := make(chan struct{})
	cancelCount := atomic.Int32{}
	cancel := func() {
		if cancelCount.Add(1) == 1 && !skipAutoDrain {
			close(done)
		}
	}
	mc := &managedCity{
		name:   name,
		cancel: cancel,
		done:   done,
		cr: &CityRuntime{
			cityName: name,
			cfg: &config.City{
				Daemon: config.DaemonConfig{ShutdownTimeout: "1s"},
			},
			sp:     runtime.NewFake(),
			rec:    events.Discard,
			stdout: io.Discard,
			stderr: io.Discard,
		},
	}
	return mc, done, &cancelCount
}

func TestPerformCityRestartNotFound(t *testing.T) {
	cr := newCityRegistry()
	reconcileCh := make(chan reconcileRequest, 1)
	rec := &collectingRecorder{}

	res, err := performCityRestart(performCityRestartParams{
		Name:        "phantom",
		Force:       false,
		Timeout:     500 * time.Millisecond,
		Registry:    cr,
		ReconcileCh: reconcileCh,
		Recorder:    rec,
		BinarySHA:   func() (string, error) { return "abc", nil },
		Stderr:      io.Discard,
	})
	if err == nil {
		t.Fatalf("performCityRestart err = nil, want not_found error")
	}
	if !strings.Contains(err.Error(), "not_found") && !strings.Contains(err.Error(), "phantom") {
		t.Fatalf("err = %v, want not_found/phantom detail", err)
	}
	if res.Forced {
		t.Fatalf("res.Forced = true on not-found path, want false")
	}
}

func TestPerformCityRestartGracefulDrain(t *testing.T) {
	cr := newCityRegistry()
	mc, _, cancels := fakeManagedCity(t, "trader", false)
	cr.Add("/trader", mc)

	reconcileCh := make(chan reconcileRequest, 1)
	rec := &collectingRecorder{}
	shaSeq := []string{"old-binary", "new-binary"}
	shaIdx := 0
	binarySHA := func() (string, error) {
		v := shaSeq[shaIdx]
		shaIdx++
		return v, nil
	}

	res, err := performCityRestart(performCityRestartParams{
		Name:        "trader",
		Force:       false,
		Timeout:     2 * time.Second,
		Registry:    cr,
		ReconcileCh: reconcileCh,
		Recorder:    rec,
		BinarySHA:   binarySHA,
		Stderr:      io.Discard,
	})
	if err != nil {
		t.Fatalf("performCityRestart err = %v", err)
	}
	if res.Forced {
		t.Fatalf("res.Forced = true, want false on graceful drain")
	}
	if res.OldBinarySHA != "old-binary" || res.NewBinarySHA != "new-binary" {
		t.Fatalf("res SHAs = %q/%q, want old-binary/new-binary", res.OldBinarySHA, res.NewBinarySHA)
	}
	if cancels.Load() < 1 {
		t.Fatalf("cancel never invoked")
	}
	select {
	case <-reconcileCh:
	case <-time.After(time.Second):
		t.Fatalf("reconcile request not enqueued")
	}
	if !rec.Has("controller.restart") {
		t.Fatalf("controller.restart event not emitted; events=%v", rec.Types())
	}
}

func TestPerformCityRestartForceSkipsWait(t *testing.T) {
	cr := newCityRegistry()
	mc, done, _ := fakeManagedCity(t, "trader", true) // do NOT auto-close done
	cr.Add("/trader", mc)
	t.Cleanup(func() {
		select {
		case <-done:
		default:
			close(done)
		}
	})

	reconcileCh := make(chan reconcileRequest, 1)
	rec := &collectingRecorder{}
	binarySHA := func() (string, error) { return "sha", nil }

	start := time.Now()
	res, err := performCityRestart(performCityRestartParams{
		Name:        "trader",
		Force:       true,
		Timeout:     10 * time.Second,
		Registry:    cr,
		ReconcileCh: reconcileCh,
		Recorder:    rec,
		BinarySHA:   binarySHA,
		Stderr:      io.Discard,
	})
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("performCityRestart err = %v", err)
	}
	if !res.Forced {
		t.Fatalf("res.Forced = false, want true under --force")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("force path waited %s; expected near-immediate return", elapsed)
	}
}

func TestPerformCityRestartTimeoutForces(t *testing.T) {
	cr := newCityRegistry()
	mc, done, _ := fakeManagedCity(t, "trader", true) // do NOT auto-close done
	cr.Add("/trader", mc)
	t.Cleanup(func() {
		select {
		case <-done:
		default:
			close(done)
		}
	})

	reconcileCh := make(chan reconcileRequest, 1)
	rec := &collectingRecorder{}
	binarySHA := func() (string, error) { return "sha", nil }

	start := time.Now()
	res, err := performCityRestart(performCityRestartParams{
		Name:        "trader",
		Force:       false,
		Timeout:     150 * time.Millisecond,
		Registry:    cr,
		ReconcileCh: reconcileCh,
		Recorder:    rec,
		BinarySHA:   binarySHA,
		Stderr:      io.Discard,
	})
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("performCityRestart err = %v", err)
	}
	if !res.Forced {
		t.Fatalf("res.Forced = false, want true after drain timeout")
	}
	if elapsed < 150*time.Millisecond {
		t.Fatalf("returned in %s before timeout fired", elapsed)
	}
}

func TestPerformCityRestartLeavesSiblingsUntouched(t *testing.T) {
	cr := newCityRegistry()
	target, _, _ := fakeManagedCity(t, "trader", false)
	sibling, _, siblingCancels := fakeManagedCity(t, "jarvis", true)
	cr.Add("/trader", target)
	cr.Add("/jarvis", sibling)
	t.Cleanup(func() {
		select {
		case <-sibling.done:
		default:
			close(sibling.done)
		}
	})

	reconcileCh := make(chan reconcileRequest, 1)
	rec := &collectingRecorder{}
	binarySHA := func() (string, error) { return "sha", nil }

	if _, err := performCityRestart(performCityRestartParams{
		Name:        "trader",
		Force:       false,
		Timeout:     2 * time.Second,
		Registry:    cr,
		ReconcileCh: reconcileCh,
		Recorder:    rec,
		BinarySHA:   binarySHA,
		Stderr:      io.Discard,
	}); err != nil {
		t.Fatalf("performCityRestart err = %v", err)
	}

	if siblingCancels.Load() != 0 {
		t.Fatalf("sibling cancel invoked %d times, want 0", siblingCancels.Load())
	}
	if cr.Has("/trader") {
		t.Fatalf("trader still registered after restart drain; reconcile must repopulate it")
	}
	if !cr.Has("/jarvis") {
		t.Fatalf("jarvis was unregistered, but only trader should have been touched")
	}
}

// --- handleSupervisorConn integration ---------------------------------------

// TestHandleSupervisorConnRestartCity verifies the socket handler
// dispatches a restart-city line to the restart channel and writes
// the helper's reply back to the client.
func TestHandleSupervisorConnRestartCity(t *testing.T) {
	restartCh := make(chan restartCityRequest, 1)

	// Server: spin up a one-shot listener bound to handleSupervisorConn.
	// Unix sockets cap path length, so use the short temp helper that
	// other supervisor socket tests already rely on.
	dir := shortTempDir(t, "gc-rc-")
	sock := filepath.Join(dir, "t.sock")
	lis, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer lis.Close() //nolint:errcheck
	t.Cleanup(func() { os.Remove(sock) }) //nolint:errcheck

	go func() {
		conn, err := lis.Accept()
		if err != nil {
			return
		}
		handleSupervisorConn(conn, func(supervisorShutdownMode) {}, nil, restartCh, nil)
	}()

	// Stub the responder: pretend the supervisor's main loop processed
	// the request and ack'd with formatted reply.
	go func() {
		req := <-restartCh
		req.reply <- restartCityResponse{
			OldBinarySHA: "old",
			NewBinarySHA: "new",
			DrainMs:      42,
			Forced:       false,
		}
	}()

	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close() //nolint:errcheck

	if _, err := c.Write([]byte("restart-city trader force=false timeout_ms=60000\n")); err != nil {
		t.Fatal(err)
	}
	c.SetReadDeadline(time.Now().Add(2 * time.Second)) //nolint:errcheck
	br := bufio.NewReader(c)
	resp, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("ReadString: %v (resp=%q)", err, resp)
	}
	old, new, drainMs, forced, err := parseRestartCityReply(strings.TrimSpace(resp))
	if err != nil {
		t.Fatalf("parseRestartCityReply: %v", err)
	}
	if old != "old" || new != "new" || drainMs != 42 || forced {
		t.Fatalf("reply round-trip mismatch: old=%q new=%q drain=%d forced=%v", old, new, drainMs, forced)
	}
}

// --- helpers ----------------------------------------------------------------

type collectingRecorder struct {
	mu     sync.Mutex
	events []events.Event
}

func (r *collectingRecorder) Record(e events.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

func (r *collectingRecorder) Has(typ string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.events {
		if e.Type == typ {
			return true
		}
	}
	return false
}

func (r *collectingRecorder) Types() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.events))
	for _, e := range r.events {
		out = append(out, e.Type)
	}
	return out
}
