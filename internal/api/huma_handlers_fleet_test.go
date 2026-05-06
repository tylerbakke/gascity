package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/citylayout"
)

// Fixture extracted verbatim from the fleet-status order's emitted
// snapshot. Mirrors command-center's runtime payload — three hosts
// (one local primary, one unreachable worker, one DR target reached
// via public-fallback) plus the typed summary.
const fleetStatusFixtureJSON = `{
  "generated_at": "2026-05-06T02:31:03Z",
  "hosts": [
    {
      "event": "fleet.host",
      "host": "laptop",
      "address": "127.0.0.1",
      "address_used": "127.0.0.1",
      "role": "primary-control-plane",
      "via": "local",
      "reachable": true,
      "gc_version": "1.0.1",
      "supervisor_running": true,
      "sessions_active": 22,
      "dolt_servers": 3,
      "mem_free_gb": 6.54,
      "disk_free_gb": 195,
      "error": null
    },
    {
      "event": "fleet.host",
      "host": "dkubex",
      "address": "100.64.135.117",
      "address_used": "100.64.135.117",
      "role": "worker",
      "via": "unreachable",
      "reachable": false,
      "gc_version": null,
      "supervisor_running": null,
      "sessions_active": null,
      "dolt_servers": null,
      "mem_free_gb": null,
      "disk_free_gb": null,
      "error": "ssh probe failed (user=root primary=100.64.135.117 public=none)"
    },
    {
      "event": "fleet.host",
      "host": "dolt-remote-1",
      "address": "100.79.35.95",
      "address_used": "5.161.48.103",
      "role": "dr-target",
      "via": "public-fallback",
      "reachable": true,
      "gc_version": null,
      "supervisor_running": null,
      "sessions_active": null,
      "dolt_servers": 0,
      "mem_free_gb": 0,
      "disk_free_gb": 32,
      "error": null
    }
  ],
  "summary": {
    "event": "fleet.summary",
    "total": 3,
    "reachable": 2,
    "unreachable": 1,
    "generated_at": "2026-05-06T02:31:03Z"
  }
}
`

// writeFleetStatusFixture writes contents to the canonical snapshot
// location under the fakeState's CityPath. generated_at is left to
// the caller-supplied JSON so freshness/staleness paths can be
// exercised independently.
func writeFleetStatusFixture(t *testing.T, state *fakeState, contents string) string {
	t.Helper()
	runtimeDir := citylayout.RuntimePath(state.CityPath(), "runtime")
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		t.Fatalf("mkdir runtime dir: %v", err)
	}
	path := filepath.Join(runtimeDir, fleetStatusFilename)
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("write snapshot: %v", err)
	}
	return path
}

func TestFleetStatusHandlerHappyPath(t *testing.T) {
	state := newFakeState(t)
	// Rewrite generated_at to "now" so the snapshot is fresh and the
	// stale flag stays false. The fixture's hard-coded timestamp is
	// far in the past relative to most CI runs.
	now := time.Now().UTC().Truncate(time.Second)
	fresh := strings.ReplaceAll(fleetStatusFixtureJSON,
		`"2026-05-06T02:31:03Z"`,
		`"`+now.Format(time.RFC3339)+`"`)
	writeFleetStatusFixture(t, state, fresh)

	h := newTestCityHandler(t, state)
	req := httptest.NewRequest(http.MethodGet, cityURL(state, "/fleet/status"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	var got FleetStatusBody
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v; body=%s", err, rec.Body.String())
	}
	if len(got.Hosts) != 3 {
		t.Fatalf("hosts = %d, want 3", len(got.Hosts))
	}
	if got.Summary.Total != 3 || got.Summary.Reachable != 2 || got.Summary.Unreachable != 1 {
		t.Fatalf("summary = %+v, want total=3 reachable=2 unreachable=1", got.Summary)
	}
	if got.Stale {
		t.Fatalf("stale = true, want false for fresh snapshot")
	}
	if got.AgeSec < 0 || got.AgeSec > 60 {
		t.Fatalf("age_sec = %d, want a small positive value", got.AgeSec)
	}

	// Spot-check that JSON-null probe fields decoded as nil pointers
	// (not zero-valued ints/floats), so the wire stays honest about
	// "we don't know" vs "we measured zero".
	dkubex := got.Hosts[1]
	if dkubex.Host != "dkubex" {
		t.Fatalf("hosts[1].host = %q, want dkubex", dkubex.Host)
	}
	if dkubex.GCVersion != nil {
		t.Fatalf("dkubex gc_version = %v, want nil", *dkubex.GCVersion)
	}
	if dkubex.MemFreeGB != nil {
		t.Fatalf("dkubex mem_free_gb = %v, want nil", *dkubex.MemFreeGB)
	}
	if dkubex.Error == nil || *dkubex.Error == "" {
		t.Fatalf("dkubex error = nil/empty, want a probe error string")
	}

	// dolt-remote-1's dolt_servers is 0 (not null) — the probe
	// reached the host but found zero managed dolt servers. This
	// distinction matters for the dashboard's rendering logic.
	doltRemote := got.Hosts[2]
	if doltRemote.DoltServers == nil || *doltRemote.DoltServers != 0 {
		t.Fatalf("dolt-remote-1 dolt_servers = %v, want 0 (non-null)", doltRemote.DoltServers)
	}
}

func TestFleetStatusHandlerMissingFile(t *testing.T) {
	state := newFakeState(t)
	// Intentionally do NOT write the snapshot file. The runtime dir
	// itself doesn't need to exist either; missing parent is the
	// same observable outcome as missing file (both produce
	// fs.ErrNotExist).

	h := newTestCityHandler(t, state)
	req := httptest.NewRequest(http.MethodGet, cityURL(state, "/fleet/status"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "snapshot_missing") {
		t.Fatalf("body = %q, want snapshot_missing detail", rec.Body.String())
	}
}

func TestFleetStatusHandlerMalformedJSON(t *testing.T) {
	state := newFakeState(t)
	writeFleetStatusFixture(t, state, `{"generated_at": "not-a-timestamp", "hosts": [`)

	h := newTestCityHandler(t, state)
	req := httptest.NewRequest(http.MethodGet, cityURL(state, "/fleet/status"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "snapshot_malformed") {
		t.Fatalf("body = %q, want snapshot_malformed detail", rec.Body.String())
	}
}

func TestFleetStatusHandlerStaleSnapshot(t *testing.T) {
	state := newFakeState(t)
	// generated_at far in the past relative to the freshness window.
	stale := time.Now().UTC().Add(-2 * fleetStatusFreshnessWindow).Format(time.RFC3339)
	contents := strings.ReplaceAll(fleetStatusFixtureJSON,
		`"2026-05-06T02:31:03Z"`,
		`"`+stale+`"`)
	writeFleetStatusFixture(t, state, contents)

	h := newTestCityHandler(t, state)
	req := httptest.NewRequest(http.MethodGet, cityURL(state, "/fleet/status"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (stale data is still served); body=%s", rec.Code, rec.Body.String())
	}
	var got FleetStatusBody
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !got.Stale {
		t.Fatalf("stale = false, want true for old snapshot")
	}
	if got.AgeSec < int(fleetStatusFreshnessWindow.Seconds()) {
		t.Fatalf("age_sec = %d, want >= %d", got.AgeSec, int(fleetStatusFreshnessWindow.Seconds()))
	}
	if len(got.Hosts) != 3 {
		t.Fatalf("hosts = %d, want 3 (stale data is still returned)", len(got.Hosts))
	}
}
