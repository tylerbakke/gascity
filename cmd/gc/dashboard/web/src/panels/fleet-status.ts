// Fleet Status panel: typed read-side projection of the snapshot
// the fleet-status order writes under <city>/.gc/runtime/fleet-status.json.
//
// The order ticks at 5-minute cadence and probes every host on the
// fleet inventory (Tailscale primary, public-fallback when needed,
// "unreachable" when neither path works). The panel renders one row
// per host with reachability + per-host probe data, and surfaces
// the snapshot's age + freshness up front so operators can tell at
// a glance whether the data they're looking at is stale.
//
// The handler returns 503 when the order has not produced the
// snapshot yet (legitimate "first boot, never ticked" state) and
// 502 when the snapshot is malformed; the panel renders both as
// distinct error states rather than silently showing an empty table.
//
// V1 polling cadence: 60s. SSE migration is tracked separately so
// the panel can react to fleet.host / fleet.summary events instead
// of polling once the bus carries them.

import { api, cityScope } from "../api";
import type { DashboardSchema } from "../api";
import { byId, clear, el } from "../util/dom";
import { formatTimestamp } from "../util/legacy";

type FleetStatusBody = DashboardSchema["FleetStatusBody"];
type FleetHost = DashboardSchema["FleetHost"];

const PANEL_ID = "fleet-panel";
const COUNT_ID = "fleet-count";
const BODY_ID = "fleet-body";

export async function renderFleetStatus(): Promise<void> {
  const panel = byId(PANEL_ID);
  const body = byId(BODY_ID);
  const count = byId(COUNT_ID);
  if (!panel || !body || !count) return;

  const city = cityScope();
  if (!city) {
    panel.hidden = true;
    return;
  }
  panel.hidden = false;

  const { data, error, response } = await api.GET("/v0/city/{cityName}/fleet/status", {
    params: { path: { cityName: city } },
  });

  clear(body);
  if (error || !data) {
    renderFleetError(body, count, response?.status, error);
    return;
  }
  renderFleetTable(body, count, data);
}

function renderFleetError(
  body: HTMLElement,
  count: HTMLElement,
  status: number | undefined,
  error: unknown,
): void {
  count.textContent = "n/a";
  const detail = errorDetail(error);
  let title: string;
  let hint: string;
  switch (status) {
    case 503:
      title = "Fleet snapshot not yet available";
      hint =
        "The fleet-status order writes the snapshot every 5 minutes. " +
        "Check that the order is registered and has run at least once.";
      break;
    case 502:
      title = "Fleet snapshot is malformed";
      hint =
        "The on-disk snapshot did not parse against the fleet schema. " +
        "Inspect <city>/.gc/runtime/fleet-status.json on the host.";
      break;
    default:
      title = "Fleet status unavailable";
      hint = detail || "Unable to fetch the fleet snapshot from the supervisor.";
  }
  body.append(
    el("div", { class: "empty-state" }, [
      el("p", {}, [el("strong", {}, [title])]),
      el("p", {}, [hint]),
    ]),
  );
}

function errorDetail(error: unknown): string {
  if (!error) return "";
  if (typeof error === "string") return error;
  if (typeof error === "object" && error !== null) {
    const detail = (error as { detail?: unknown }).detail;
    if (typeof detail === "string") return detail;
    const message = (error as { message?: unknown }).message;
    if (typeof message === "string") return message;
  }
  return "";
}

function renderFleetTable(
  body: HTMLElement,
  count: HTMLElement,
  data: FleetStatusBody,
): void {
  const hosts: FleetHost[] = data.hosts ?? [];
  count.textContent = String(hosts.length);

  body.append(renderFleetHeader(data));
  if (data.stale) {
    body.append(renderStaleBanner(data.age_sec));
  }

  if (hosts.length === 0) {
    body.append(
      el("div", { class: "empty-state" }, [
        el("p", {}, ["Fleet snapshot present but lists zero hosts."]),
      ]),
    );
    return;
  }

  const table = el("table", { class: "fleet-table" });
  const thead = el("thead", {}, [
    el("tr", {}, [
      el("th", {}, ["Host"]),
      el("th", {}, ["Role"]),
      el("th", {}, ["Reachable"]),
      el("th", {}, ["Via"]),
      el("th", {}, ["Address used"]),
      el("th", {}, ["gc"]),
      el("th", { class: "fleet-col-num" }, ["Sessions"]),
      el("th", { class: "fleet-col-num" }, ["Dolt"]),
      el("th", { class: "fleet-col-num" }, ["Mem (GiB)"]),
      el("th", { class: "fleet-col-num" }, ["Disk (GiB)"]),
      el("th", {}, ["Notes"]),
    ]),
  ]);
  const tbody = el("tbody");
  for (const host of hosts) {
    tbody.append(renderHostRow(host));
  }
  table.append(thead, tbody);
  body.append(table);
}

function renderFleetHeader(data: FleetStatusBody): HTMLElement {
  const summary = data.summary;
  const generated = data.generated_at ? formatTimestamp(data.generated_at) : "—";
  const ageLabel = formatAge(data.age_sec);
  return el("div", { class: "fleet-header" }, [
    el("div", { class: "fleet-summary-stats" }, [
      summaryChip(String(summary?.total ?? 0), "Total"),
      summaryChip(String(summary?.reachable ?? 0), "Reachable"),
      summaryChip(String(summary?.unreachable ?? 0), "Unreachable"),
    ]),
    el("div", { class: "fleet-meta" }, [
      el("span", { class: "fleet-meta-item" }, [`Snapshot: ${generated}`]),
      el("span", { class: "fleet-meta-item" }, [`Age: ${ageLabel}`]),
    ]),
  ]);
}

function renderStaleBanner(ageSec: number): HTMLElement {
  return el("div", { class: "alert-item alert-yellow fleet-stale-banner" }, [
    `Snapshot is stale (age ${formatAge(ageSec)}). Rendering last-known data.`,
  ]);
}

function summaryChip(value: string, label: string): HTMLElement {
  return el("div", { class: "stat" }, [
    el("span", { class: "stat-value" }, [value]),
    el("span", { class: "stat-label" }, [label]),
  ]);
}

function renderHostRow(host: FleetHost): HTMLElement {
  const reachableClass = host.reachable ? "fleet-mark-yes" : "fleet-mark-no";
  const reachableMark = host.reachable ? "\u2713" : "\u2717";
  const errorNote = host.error ?? "";
  return el(
    "tr",
    { class: host.reachable ? "" : "fleet-row-unreachable" },
    [
      el("td", {}, [el("strong", {}, [host.host])]),
      el("td", {}, [host.role || "—"]),
      el("td", { class: reachableClass }, [reachableMark]),
      el("td", {}, [host.via || "—"]),
      el("td", { class: "fleet-col-mono" }, [host.address_used || host.address || "—"]),
      el("td", {}, [host.gc_version ?? "—"]),
      el("td", { class: "fleet-col-num" }, [renderNullableInt(host.sessions_active)]),
      el("td", { class: "fleet-col-num" }, [renderNullableInt(host.dolt_servers)]),
      el("td", { class: "fleet-col-num" }, [renderNullableFloat(host.mem_free_gb)]),
      el("td", { class: "fleet-col-num" }, [renderNullableFloat(host.disk_free_gb)]),
      el("td", { class: "fleet-col-note" }, [errorNote]),
    ],
  );
}

function renderNullableInt(value: number | null | undefined): string {
  if (value === null || value === undefined) return "—";
  return String(value);
}

function renderNullableFloat(value: number | null | undefined): string {
  if (value === null || value === undefined) return "—";
  return value.toFixed(1);
}

function formatAge(seconds: number | undefined): string {
  if (seconds === undefined || seconds < 0) return "—";
  if (seconds < 60) return `${seconds}s`;
  if (seconds < 3600) return `${Math.floor(seconds / 60)}m`;
  if (seconds < 86_400) {
    const hours = Math.floor(seconds / 3600);
    const minutes = Math.floor((seconds % 3600) / 60);
    return minutes > 0 ? `${hours}h ${minutes}m` : `${hours}h`;
  }
  return `${Math.floor(seconds / 86_400)}d`;
}
