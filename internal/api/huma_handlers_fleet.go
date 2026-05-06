package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gastownhall/gascity/internal/citylayout"
)

// fleetStatusFilename is the snapshot file the fleet-status order
// writes under the city's runtime directory. The order owns this
// path; the dashboard's API surface is a typed read-side projection
// of whatever the order most recently wrote.
const fleetStatusFilename = "fleet-status.json"

// fleetStatusFreshnessWindow is the maximum age before the snapshot
// is reported as stale on the wire. The fleet-status order ticks at
// 5-minute cadence; 30 minutes is six ticks of grace and matches the
// "honest reporting beats hiding it" contract spelled out on the
// cc-control-plane-dashboard bead.
const fleetStatusFreshnessWindow = 30 * time.Minute

// humaHandleFleetStatus serves the read-side projection of the
// fleet-status snapshot for the running city. The fleet-status order
// (registered in the city's pack) probes every host on its inventory
// and atomically rewrites <city>/.gc/runtime/fleet-status.json on
// each tick. This handler reads the snapshot, parses it through a
// typed schema (no map[string]any on the wire), and returns one of:
//
//   - 200 with the parsed snapshot when the file is present and
//     well-formed. The Stale flag is true when the snapshot is older
//     than fleetStatusFreshnessWindow; clients can render a stale
//     banner without losing the underlying data.
//   - 503 Service Unavailable when the snapshot file does not exist
//     yet. This is the legitimate "the order hasn't run since this
//     city booted" case; an empty 200 response would mislead callers.
//   - 502 Bad Gateway when the snapshot file exists but cannot be
//     parsed against the typed schema. The order is the upstream that
//     produced the broken payload, so 502 is the semantically correct
//     status. Returning 200 with a partial body would defeat the
//     typed-wire contract (the dashboard would have to hand-decode
//     the rest).
//   - 500 Internal Server Error for unexpected I/O failures (permission
//     denied, EIO). These are local-side failures, not snapshot
//     producer failures.
//
// Path resolution is per-running-city: the fleet-status order writes
// to the city scope it executed in, so the dashboard reads from the
// same per-city runtime directory. Cross-city snapshot inspection is
// out of scope for v1; users select the city via the dashboard's
// city picker just like every other per-city panel.
func (s *Server) humaHandleFleetStatus(_ context.Context, _ *FleetStatusInput) (*FleetStatusOutput, error) {
	cityRoot := s.state.CityPath()
	if cityRoot == "" {
		return nil, huma.Error500InternalServerError("internal: city path is unset on this server")
	}
	snapshotPath := citylayout.RuntimePath(cityRoot, "runtime", fleetStatusFilename)

	raw, err := os.ReadFile(snapshotPath)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, huma.Error503ServiceUnavailable(
			"snapshot_missing: fleet-status order has not produced " + snapshotPath +
				" yet; check that the order is registered and has ticked at least once",
		)
	case err != nil:
		return nil, huma.Error500InternalServerError(fmt.Sprintf("read snapshot %s: %v", snapshotPath, err))
	}

	body, err := parseFleetStatusSnapshot(raw)
	if err != nil {
		return nil, huma.Error502BadGateway(fmt.Sprintf("snapshot_malformed: parsing %s: %v", snapshotPath, err))
	}

	now := time.Now().UTC()
	age := now.Sub(body.GeneratedAt)
	if age < 0 {
		age = 0
	}
	body.AgeSec = int(age.Round(time.Second).Seconds())
	body.Stale = age > fleetStatusFreshnessWindow

	return &FleetStatusOutput{Body: body}, nil
}

// parseFleetStatusSnapshot decodes the on-disk snapshot into the
// typed wire body. DisallowUnknownFields is intentionally NOT set:
// the fleet-status order may grow new optional probe fields ahead
// of this handler, and rejecting unknown fields would force a
// lock-step deploy between the order and the dashboard. The handler
// owns the typed wire shape; new producer fields are silently
// ignored until the wire schema catches up.
func parseFleetStatusSnapshot(raw []byte) (FleetStatusBody, error) {
	var body FleetStatusBody
	if err := json.NewDecoder(bytes.NewReader(raw)).Decode(&body); err != nil {
		return FleetStatusBody{}, err
	}
	return body, nil
}
