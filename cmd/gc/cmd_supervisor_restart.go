package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/spf13/cobra"
)

// supervisorRestartCityDefaultTimeout is the graceful drain budget when
// the user does not specify --timeout. Five minutes mirrors the existing
// reload reply timeout and gives in-flight polecat work time to wrap up.
var supervisorRestartCityDefaultTimeout = 5 * time.Minute

// supervisorRestartCityDefaultRespawnTimeout bounds how long
// performCityRestart waits for the city to reappear in the registry
// after reconcile has been triggered. ci-iczpa: without this guard the
// CLI returned "ok" even when the controller never respawned, because
// the wire reply was sent right after drain completed. Sixty seconds
// matches the controller startup budget operators tolerate before
// considering a respawn dead.
var supervisorRestartCityDefaultRespawnTimeout = 60 * time.Second

// supervisorRestartCityRespawnPollInterval bounds how aggressively the
// wait loop scans the registry snapshot for a respawned city. Kept
// short so respawn is observed quickly without dominating CPU.
var supervisorRestartCityRespawnPollInterval = 50 * time.Millisecond

// restartCityRequest is enqueued onto restartCityCh by the socket handler
// and consumed by the supervisor's main loop. The reply channel returns
// the helper's structured result so the socket handler can emit a
// formatted reply to the client.
type restartCityRequest struct {
	name    string
	force   bool
	timeout time.Duration
	reply   chan restartCityResponse
}

// restartCityResponse carries the result of performCityRestart back to
// the socket handler. Err is non-nil when the city is not registered,
// the binary SHA could not be hashed, or the drain/reconcile sequence
// failed; the formatted reply uses Err's text.
type restartCityResponse struct {
	OldBinarySHA string
	NewBinarySHA string
	DrainMs      int64
	Forced       bool
	Err          error
}

// performCityRestartParams bundles the dependencies performCityRestart
// needs so the helper is unit-testable without spinning up a real
// supervisor / city goroutine.
//
// RespawnTimeout, when positive, makes performCityRestart wait for the
// city to reappear in the registry after reconcile fires. A zero or
// negative value skips the wait — convenient for unit tests that don't
// model reconcile but want to exercise the drain path directly. The
// production supervisor (dispatchRestartCity) always sets a positive
// default so ci-iczpa cannot regress.
//
// OnReconcile lets the caller run reconcile synchronously between the
// drain and the respawn wait. The supervisor's main loop runs reconcile
// itself (its goroutine drives the whole loop), so it passes a closure
// that invokes safeReconcile directly; tests can leave it nil.
type performCityRestartParams struct {
	Name           string
	Force          bool
	Timeout        time.Duration
	RespawnTimeout time.Duration
	Registry       *cityRegistry
	ReconcileCh    chan reconcileRequest
	OnReconcile    func()
	Recorder       events.Recorder
	BinarySHA      func() (string, error)
	Stderr         io.Writer
}

// performCityRestartResult is the success-path output of
// performCityRestart: the binary SHAs measured before and after the
// drain, the wall time spent draining, and whether the drain was
// forced (either by --force or by exhausting the timeout).
type performCityRestartResult struct {
	OldBinarySHA string
	NewBinarySHA string
	DrainDur     time.Duration
	Forced       bool
}

// performCityRestart drains the named city's controller, removes it
// from the registry, triggers a reconcile so the supervisor's main
// loop respawns it on the current on-disk gc binary, and emits a
// controller.restart event. Sibling cities are untouched.
//
// Force semantics: when params.Force is true the drain wait is
// skipped and the city's runtime is shut down immediately; the
// returned Forced flag is true. When Force is false but the drain
// does not complete within params.Timeout, performCityRestart falls
// back to a forced shutdown and returns Forced=true so callers can
// surface the distinction.
func performCityRestart(params performCityRestartParams) (performCityRestartResult, error) {
	if params.Name == "" {
		return performCityRestartResult{}, fmt.Errorf("performCityRestart: empty city name")
	}
	if params.Registry == nil {
		return performCityRestartResult{}, fmt.Errorf("performCityRestart: nil registry")
	}
	if params.BinarySHA == nil {
		params.BinarySHA = func() (string, error) { return "", nil }
	}
	if params.Stderr == nil {
		params.Stderr = io.Discard
	}

	// Locate the named city in the registry by walking the snapshot.
	// The cities map is keyed by path; performCityRestart's caller
	// addresses cities by name so it matches the bead vocabulary.
	var (
		path string
		mc   *managedCity
	)
	params.Registry.ReadCallback(func(
		cities map[string]*managedCity,
		_ map[string]cityInitProgress,
		_ map[string]*initFailRecord,
		_ map[string]*panicRecord,
	) {
		for p, candidate := range cities {
			if candidate != nil && candidate.name == params.Name {
				path = p
				mc = candidate
				return
			}
		}
	})
	if mc == nil {
		return performCityRestartResult{}, fmt.Errorf("not_found: city %q is not currently managed by the supervisor", params.Name)
	}

	oldSHA, _ := params.BinarySHA()

	// Detach from the registry under lock so reconcile sees the slot
	// vacated and starts a fresh goroutine without racing against the
	// drain. tombstoned signals teardown so any concurrent shutdown
	// path skips the same managedCity.
	params.Registry.BatchUpdate(func(
		cities map[string]*managedCity,
		_ map[string]cityInitProgress,
		_ map[string]*initFailRecord,
		_ map[string]*panicRecord,
	) {
		if cur, ok := cities[path]; ok && cur == mc {
			mc.tombstoned.Store(true)
			delete(cities, path)
		}
	})

	drainStart := time.Now()
	forced := drainAndStopCity(mc, path, params.Timeout, params.Force, params.Stderr)
	drainDur := time.Since(drainStart)

	// Trigger reconcile so the supervisor brings the city back up on
	// the current on-disk binary. The queued reconcileRequest covers
	// asynchronous callers; the inline OnReconcile hook lets the
	// supervisor's main loop run reconcile right now (we are already
	// holding the loop's goroutine, so a queued request would only be
	// drained after we return — too late to verify the respawn).
	if params.ReconcileCh != nil {
		select {
		case params.ReconcileCh <- reconcileRequest{}:
		default:
		}
	}
	if params.OnReconcile != nil {
		params.OnReconcile()
	}

	newSHA, _ := params.BinarySHA()

	// Wait for the controller to come back. ci-iczpa: production saw
	// `restart-city` hang and silently exit 0 even though the city had
	// been drained and never respawned. By waiting for the city to
	// reappear in the registry (or the timeout to elapse), we can
	// surface a respawn_timeout error so the CLI exits non-zero with
	// a clear message instead of pretending success.
	if params.RespawnTimeout > 0 {
		if !waitForCityRespawn(params.Registry, params.Name, params.RespawnTimeout) {
			return performCityRestartResult{
					OldBinarySHA: oldSHA,
					NewBinarySHA: newSHA,
					DrainDur:     drainDur,
					Forced:       forced,
				}, fmt.Errorf(
					"respawn_timeout: city %q did not respawn within %s after drain; check supervisor logs for init-failure backoff",
					params.Name, params.RespawnTimeout,
				)
		}
	}

	if params.Recorder != nil {
		payload, _ := json.Marshal(api.ControllerRestartPayload{
			City:            params.Name,
			OldBinarySHA:    oldSHA,
			NewBinarySHA:    newSHA,
			Forced:          forced,
			DrainDurationMs: drainDur.Milliseconds(),
		})
		params.Recorder.Record(events.Event{
			Type:    events.ControllerRestart,
			Actor:   "gc",
			Subject: params.Name,
			Payload: payload,
		})
	}

	return performCityRestartResult{
		OldBinarySHA: oldSHA,
		NewBinarySHA: newSHA,
		DrainDur:     drainDur,
		Forced:       forced,
	}, nil
}

// waitForCityRespawn polls the registry snapshot until a non-tombstoned
// city with the given name appears or timeout elapses. Returns true on
// observed respawn, false on timeout. Lock-free read path via the
// atomic snapshot — safe to call concurrently with reconcile.
func waitForCityRespawn(cr *cityRegistry, name string, timeout time.Duration) bool {
	if cr == nil || timeout <= 0 {
		return false
	}
	poll := supervisorRestartCityRespawnPollInterval
	if poll <= 0 {
		poll = 50 * time.Millisecond
	}
	if poll > timeout {
		poll = timeout / 4
		if poll <= 0 {
			poll = time.Millisecond
		}
	}
	deadline := time.Now().Add(timeout)
	for {
		snap := cr.Snapshot()
		if snap != nil {
			if view, ok := snap.byName[name]; ok && view != nil && !view.Tombstoned {
				return true
			}
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(poll)
	}
}

// drainAndStopCity cancels the managed city's context, optionally waits
// for the city goroutine to drain in-flight sessions up to timeout, and
// forces a shutdown if drain doesn't ack in time (or if force is set).
// Returns true when the stop was forced (no drain ack or --force).
func drainAndStopCity(mc *managedCity, cityPath string, timeout time.Duration, force bool, stderr io.Writer) bool {
	if mc == nil {
		return false
	}
	mc.cancel()
	if force {
		forceShutdownCity(mc, cityPath, stderr)
		return true
	}
	if timeout <= 0 {
		// A zero or negative timeout means "no graceful wait" — fall
		// through to a forced shutdown. Treat the same as force=true
		// from the caller's perspective.
		forceShutdownCity(mc, cityPath, stderr)
		return true
	}
	select {
	case <-mc.done:
		// Graceful drain. Best-effort cleanup of the bead provider
		// and file recorder mirrors stopManagedCity.
		if err := shutdownBeadsProvider(cityPath); err != nil {
			fmt.Fprintf(stderr, "gc supervisor: city %q: bead store: %v\n", mc.name, err) //nolint:errcheck
		}
		if mc.closer != nil {
			mc.closer.Close() //nolint:errcheck
		}
		return false
	case <-time.After(timeout):
		fmt.Fprintf(stderr, "gc supervisor: city %q drain exceeded %s; forcing shutdown\n", mc.name, timeout) //nolint:errcheck
		forceShutdownCity(mc, cityPath, stderr)
		return true
	}
}

// forceShutdownCity invokes the city runtime's shutdown method (recovering
// from any panic so a buggy stop path can never bring down the
// supervisor) and tears down the bead provider / file recorder. The
// recovery mirrors the safety wrapper in stopManagedCity.
func forceShutdownCity(mc *managedCity, cityPath string, stderr io.Writer) {
	if mc.cr != nil {
		func() {
			defer func() { recover() }() //nolint:errcheck
			mc.cr.shutdown()
		}()
	}
	if err := shutdownBeadsProvider(cityPath); err != nil {
		fmt.Fprintf(stderr, "gc supervisor: city %q: bead store: %v\n", mc.name, err) //nolint:errcheck
	}
	if mc.closer != nil {
		mc.closer.Close() //nolint:errcheck
	}
}

// gcBinarySHA returns the SHA-256 hex of the running gc binary. Used as
// the BinarySHA hook in performCityRestartParams. Errors propagate so
// callers can decide whether to record an empty SHA or fail the request.
func gcBinarySHA() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locating gc binary: %w", err)
	}
	f, err := os.Open(exe)
	if err != nil {
		return "", fmt.Errorf("opening gc binary %q: %w", exe, err)
	}
	defer f.Close() //nolint:errcheck
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hashing gc binary %q: %w", exe, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// --- protocol -----------------------------------------------------------

// parseRestartCityCommand decodes a `restart-city <name> force=<bool>
// timeout_ms=<int>` line into typed fields. Returns a structured error
// when the line is malformed so callers can surface a useful "error
// <message>" reply to the client.
func parseRestartCityCommand(line string) (name string, force bool, timeout time.Duration, err error) {
	tokens := strings.Fields(strings.TrimSpace(line))
	if len(tokens) == 0 || tokens[0] != "restart-city" {
		return "", false, 0, fmt.Errorf("restart-city: missing command tag")
	}
	if len(tokens) < 4 {
		return "", false, 0, fmt.Errorf("restart-city: expected 'restart-city <name> force=<bool> timeout_ms=<int>'")
	}
	name = tokens[1]
	if name == "" {
		return "", false, 0, fmt.Errorf("restart-city: empty city name")
	}
	for _, tok := range tokens[2:] {
		k, v, ok := strings.Cut(tok, "=")
		if !ok {
			return "", false, 0, fmt.Errorf("restart-city: malformed key=value pair %q", tok)
		}
		switch k {
		case "force":
			b, parseErr := strconv.ParseBool(v)
			if parseErr != nil {
				return "", false, 0, fmt.Errorf("restart-city: force must be true|false (got %q)", v)
			}
			force = b
		case "timeout_ms":
			ms, parseErr := strconv.ParseInt(v, 10, 64)
			if parseErr != nil || ms < 0 {
				return "", false, 0, fmt.Errorf("restart-city: timeout_ms must be a non-negative integer (got %q)", v)
			}
			timeout = time.Duration(ms) * time.Millisecond
		default:
			return "", false, 0, fmt.Errorf("restart-city: unknown key %q", k)
		}
	}
	return name, force, timeout, nil
}

// formatRestartCityReply renders a successful restart-city reply line
// (without trailing newline). Wire format mirrors parseRestartCityCommand:
// space-separated key=value tokens prefixed with "ok".
func formatRestartCityReply(oldSHA, newSHA string, drainMs int64, forced bool) string {
	return fmt.Sprintf("ok old=%s new=%s drain_ms=%d forced=%t", oldSHA, newSHA, drainMs, forced)
}

// parseRestartCityReply decodes a reply line emitted by
// formatRestartCityReply. Used by the client to surface results to
// stdout and by the round-trip tests.
func parseRestartCityReply(line string) (oldSHA, newSHA string, drainMs int64, forced bool, err error) {
	tokens := strings.Fields(strings.TrimSpace(line))
	if len(tokens) == 0 || tokens[0] != "ok" {
		return "", "", 0, false, fmt.Errorf("restart-city reply: expected 'ok' prefix, got %q", line)
	}
	for _, tok := range tokens[1:] {
		k, v, ok := strings.Cut(tok, "=")
		if !ok {
			return "", "", 0, false, fmt.Errorf("restart-city reply: malformed key=value pair %q", tok)
		}
		switch k {
		case "old":
			oldSHA = v
		case "new":
			newSHA = v
		case "drain_ms":
			drainMs, err = strconv.ParseInt(v, 10, 64)
			if err != nil {
				return "", "", 0, false, fmt.Errorf("restart-city reply: drain_ms parse: %w", err)
			}
		case "forced":
			forced, err = strconv.ParseBool(v)
			if err != nil {
				return "", "", 0, false, fmt.Errorf("restart-city reply: forced parse: %w", err)
			}
		default:
			return "", "", 0, false, fmt.Errorf("restart-city reply: unknown key %q", k)
		}
	}
	return oldSHA, newSHA, drainMs, forced, nil
}

// --- supervisor main-loop dispatch -------------------------------------

// supervisorRestartCityQueueTimeout bounds how long the socket handler
// waits to enqueue a request onto restartCityCh; if the supervisor's
// main loop is wedged, the client gets a "busy" response instead of a
// hung connection.
var supervisorRestartCityQueueTimeout = 5 * time.Second

// supervisorRestartCityReplyTimeout bounds how long the socket handler
// waits for the supervisor's main loop to send back a response on the
// reply channel. Picked larger than the protocol-level drain budget so
// a graceful drain that bumps right up against its deadline still
// returns a reply instead of hanging the client.
var supervisorRestartCityReplyTimeout = 10 * time.Minute

// handleSupervisorRestartCity is the socket-side dispatcher for the
// `restart-city` protocol command. It parses the line, enqueues a
// request onto restartCityCh for the supervisor's main loop to
// process, and writes the formatted reply back to the client.
//
// Wire-format errors are reported as "error <message>"; queue
// saturation responds with "busy"; on success the reply is the line
// produced by formatRestartCityReply.
func handleSupervisorRestartCity(conn net.Conn, line string, restartCityCh chan restartCityRequest) {
	name, force, timeout, err := parseRestartCityCommand(line)
	if err != nil {
		fmt.Fprintf(conn, "error %s\n", err.Error()) //nolint:errcheck
		return
	}
	if restartCityCh == nil {
		fmt.Fprintln(conn, "error restart-city: supervisor does not have a restart channel wired") //nolint:errcheck
		return
	}
	req := restartCityRequest{
		name:    name,
		force:   force,
		timeout: timeout,
		reply:   make(chan restartCityResponse, 1),
	}
	select {
	case restartCityCh <- req:
	case <-time.After(supervisorRestartCityQueueTimeout):
		fmt.Fprintln(conn, "busy") //nolint:errcheck
		return
	}
	// Allow the supervisor a generous window: protocol timeout plus
	// the reply-timeout margin lets even forced shutdowns and the
	// downstream reconcile finish before the client gives up.
	deadline := timeout + supervisorRestartCityReplyTimeout
	if deadline <= 0 {
		deadline = supervisorRestartCityReplyTimeout
	}
	select {
	case resp := <-req.reply:
		if resp.Err != nil {
			fmt.Fprintf(conn, "error %s\n", resp.Err.Error()) //nolint:errcheck
			return
		}
		fmt.Fprintln(conn, formatRestartCityReply(resp.OldBinarySHA, resp.NewBinarySHA, resp.DrainMs, resp.Forced)) //nolint:errcheck
	case <-time.After(deadline):
		fmt.Fprintln(conn, "error restart-city: timed out waiting for supervisor reply") //nolint:errcheck
	}
}

// supervisorEventRecorderForCity returns an events.Recorder pointed at
// a city's .gc/events.jsonl. Looks the city up by name in the registry
// snapshot to find its on-disk path; returns events.Discard when the
// city is not currently managed (in which case performCityRestart will
// surface a not_found error to the client and the recorder will be
// unused). Best-effort: a failure to open the file recorder also
// degrades to events.Discard so the restart request never fails just
// because telemetry is missing.
func supervisorEventRecorderForCity(cr *cityRegistry, name string, stderr io.Writer) events.Recorder {
	if cr == nil {
		return events.Discard
	}
	snap := cr.Snapshot()
	view, ok := snap.byName[name]
	if !ok || view == nil {
		return events.Discard
	}
	fr, err := events.NewFileRecorder(filepath.Join(view.Path, ".gc", "events.jsonl"), stderr)
	if err != nil {
		return events.Discard
	}
	return fr
}

// dispatchRestartCity consumes a restart request from the supervisor's
// main loop and writes the helper's response back to req.reply. Run
// from the main reconcile loop so reconcile and restart serialize
// (we never have a half-stopped city racing reconcile).
//
// onReconcile is the closure the main loop uses to run reconcile
// synchronously between the drain and the respawn wait. It must be
// non-nil in production so ci-iczpa cannot regress; tests may pass nil
// because they don't model the reconcile loop. respawnTimeout overrides
// the default 60s wait window; pass <= 0 to use the package default.
func dispatchRestartCity(req restartCityRequest, cr *cityRegistry, reconcileCh chan reconcileRequest, onReconcile func(), respawnTimeout time.Duration, rec events.Recorder, stderr io.Writer) {
	if rec == nil {
		rec = events.Discard
	}
	if respawnTimeout <= 0 {
		respawnTimeout = supervisorRestartCityDefaultRespawnTimeout
	}
	res, err := performCityRestart(performCityRestartParams{
		Name:           req.name,
		Force:          req.force,
		Timeout:        req.timeout,
		RespawnTimeout: respawnTimeout,
		Registry:       cr,
		ReconcileCh:    reconcileCh,
		OnReconcile:    onReconcile,
		Recorder:       rec,
		BinarySHA:      gcBinarySHA,
		Stderr:         stderr,
	})
	resp := restartCityResponse{
		OldBinarySHA: res.OldBinarySHA,
		NewBinarySHA: res.NewBinarySHA,
		DrainMs:      res.DrainDur.Milliseconds(),
		Forced:       res.Forced,
		Err:          err,
	}
	select {
	case req.reply <- resp:
	default:
		// Client gave up waiting — log so operators see the lost
		// reply and stop expecting it. The work has already happened.
		fmt.Fprintf(stderr, "gc supervisor: restart-city %q: client disconnected before reply\n", req.name) //nolint:errcheck
	}
}

// --- cobra subcommand --------------------------------------------------

// newSupervisorRestartCityCmd registers the per-city hot-swap command.
// It is wired alongside the existing reload subcommand so operators can
// drop a fixed binary into a single city without taking the whole town
// offline.
func newSupervisorRestartCityCmd(stdout, stderr io.Writer) *cobra.Command {
	var force bool
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "restart-city <city-name>",
		Short: "Drain and respawn a single city's controller on the current binary",
		Long: `Drain the named city's controller, wait for in-flight sessions to
quiesce, then respawn the controller from the current on-disk gc
binary. Sibling cities are untouched.

Pass --force to skip the drain wait and SIGTERM the controller
immediately. Pass --timeout to bound the graceful drain (default 5m).`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if restartCitySupervisor(stdout, stderr, args[0], force, timeout) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "Skip drain wait and force the controller down immediately")
	cmd.Flags().DurationVar(&timeout, "timeout", supervisorRestartCityDefaultTimeout, "Maximum time to wait for graceful drain")
	return cmd
}

// --- client --------------------------------------------------------------

// restartCitySupervisor is the client-side counterpart of
// dispatchRestartCity. Mirrors the shape of reloadSupervisor: dial the
// supervisor socket, send the protocol command, parse the reply, and
// surface the result. Returns a CLI exit code (0 success, 1 error).
func restartCitySupervisor(stdout, stderr io.Writer, name string, force bool, timeout time.Duration) int {
	if timeout <= 0 {
		timeout = supervisorRestartCityDefaultTimeout
	}

	sockPath, _ := runningSupervisorSocket()
	if sockPath == "" {
		fmt.Fprintln(stderr, "gc supervisor restart-city: supervisor is not running; start it with 'gc supervisor start'") //nolint:errcheck
		return 1
	}
	conn, err := net.DialTimeout("unix", sockPath, 2*time.Second)
	if err != nil {
		fmt.Fprintln(stderr, "gc supervisor restart-city: supervisor is not running; start it with 'gc supervisor start'") //nolint:errcheck
		return 1
	}
	defer conn.Close() //nolint:errcheck

	cmd := fmt.Sprintf("restart-city %s force=%t timeout_ms=%d\n", name, force, timeout.Milliseconds())
	if _, err := conn.Write([]byte(cmd)); err != nil {
		fmt.Fprintf(stderr, "gc supervisor restart-city: write: %v\n", err) //nolint:errcheck
		return 1
	}

	// Allow generous read time: timeout for the drain itself plus a
	// small grace buffer so the supervisor has room to record the
	// event and write the reply.
	conn.SetReadDeadline(time.Now().Add(timeout + 30*time.Second)) //nolint:errcheck
	reader := bufio.NewReader(conn)
	line, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		fmt.Fprintf(stderr, "gc supervisor restart-city: read: %v\n", err) //nolint:errcheck
		return 1
	}
	line = strings.TrimSpace(line)
	switch {
	case strings.HasPrefix(line, "ok"):
		old, new, drainMs, forced, parseErr := parseRestartCityReply(line)
		if parseErr != nil {
			fmt.Fprintf(stderr, "gc supervisor restart-city: parsing reply: %v\n", parseErr) //nolint:errcheck
			return 1
		}
		marker := "drained"
		if forced {
			marker = "forced"
		}
		fmt.Fprintf(stdout, "Restarted city %q (%s in %dms; old=%s new=%s)\n", //nolint:errcheck
			name, marker, drainMs, abbrevSHA(old), abbrevSHA(new))
		return 0
	case line == "busy":
		fmt.Fprintln(stderr, "gc supervisor restart-city: supervisor is busy; try again shortly") //nolint:errcheck
		return 1
	case strings.HasPrefix(line, "error "):
		fmt.Fprintf(stderr, "gc supervisor restart-city: %s\n", strings.TrimPrefix(line, "error ")) //nolint:errcheck
		return 1
	}
	fmt.Fprintf(stderr, "gc supervisor restart-city: unexpected reply %q\n", line) //nolint:errcheck
	return 1
}

// abbrevSHA truncates a binary SHA to a short prefix for human-friendly
// output. Empty inputs flow through as "n/a" so the operator sees the
// supervisor was unable to hash the binary instead of a blank field.
func abbrevSHA(sha string) string {
	if sha == "" {
		return "n/a"
	}
	if len(sha) <= 12 {
		return sha
	}
	return sha[:12]
}
