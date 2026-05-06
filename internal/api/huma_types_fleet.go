package api

import "time"

// Per-domain Huma input/output types for the fleet-status handler
// group. Mirrors the layout of huma_handlers_fleet.go.
//
// The wire payload is the typed projection of the snapshot written
// by the fleet-status order at <city>/.gc/runtime/fleet-status.json.
// Pointer fields hold per-host probe data that is only populated
// when the host is reachable; nil maps to JSON null so the
// reachable=false vs probed-but-missing distinction stays honest
// on the wire.

// FleetStatusInput is the Huma input for GET /v0/city/{cityName}/fleet/status.
type FleetStatusInput struct {
	CityScope
}

// FleetHost is one host's row in the fleet-status snapshot. Probe
// fields are pointers so they can carry JSON null for unreachable
// hosts; the snapshot writer (the fleet-status order) emits null
// rather than zero values to avoid lying about disk/memory state on
// hosts the probe could not reach.
type FleetHost struct {
	Event             string   `json:"event" doc:"Bus event name the snapshot row came from (e.g. \"fleet.host\")."`
	Host              string   `json:"host" doc:"Logical host name (matches the fleet inventory)."`
	Address           string   `json:"address" doc:"Configured primary address (Tailscale, LAN, or DNS)."`
	AddressUsed       string   `json:"address_used" doc:"Address the probe actually reached (may differ from address when public-fallback fired)."`
	Role              string   `json:"role" doc:"Host role label (e.g. primary-control-plane, worker, dr-target)."`
	Via               string   `json:"via" doc:"Transport the probe used (local, tailscale, public-fallback, unreachable)."`
	Reachable         bool     `json:"reachable" doc:"True when the probe successfully completed."`
	GCVersion         *string  `json:"gc_version" doc:"gc binary version reported by the host. Null when unreachable or not installed."`
	SupervisorRunning *bool    `json:"supervisor_running" doc:"True when a gc supervisor is running on the host. Null when unreachable."`
	SessionsActive    *int     `json:"sessions_active" doc:"Active session count on the host. Null when unreachable."`
	DoltServers       *int     `json:"dolt_servers" doc:"Running dolt server count on the host. Null when unreachable."`
	MemFreeGB         *float64 `json:"mem_free_gb" doc:"Free memory in GiB on the host. Null when unreachable."`
	DiskFreeGB        *float64 `json:"disk_free_gb" doc:"Free disk space in GiB on the host. Null when unreachable."`
	Error             *string  `json:"error" doc:"Probe error string for unreachable hosts. Null on success."`
}

// FleetSummary is the fleet-status snapshot summary row.
type FleetSummary struct {
	Event       string    `json:"event" doc:"Bus event name (e.g. \"fleet.summary\")."`
	Total       int       `json:"total" doc:"Total host count probed."`
	Reachable   int       `json:"reachable" doc:"Number of hosts the probe successfully reached."`
	Unreachable int       `json:"unreachable" doc:"Number of hosts the probe could not reach."`
	GeneratedAt time.Time `json:"generated_at" doc:"When the snapshot was written."`
}

// FleetStatusBody is the response body for GET /v0/city/{cityName}/fleet/status.
type FleetStatusBody struct {
	GeneratedAt time.Time    `json:"generated_at" doc:"When the snapshot was written by the fleet-status order."`
	Hosts       []FleetHost  `json:"hosts" doc:"Per-host probe rows from the snapshot."`
	Summary     FleetSummary `json:"summary" doc:"Aggregated fleet summary from the snapshot."`
	Stale       bool         `json:"stale" doc:"True when the snapshot is older than the freshness window (30 minutes). The data is still returned so the dashboard can render an honest \"last seen\" rather than disappearing."`
	AgeSec      int          `json:"age_sec" doc:"Age of the snapshot in seconds, computed from generated_at."`
}

// FleetStatusOutput is the Huma output for GET /v0/city/{cityName}/fleet/status.
type FleetStatusOutput struct {
	Body FleetStatusBody
}
