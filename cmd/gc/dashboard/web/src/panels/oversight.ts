// Oversight panel: a supervisor-scope view that lets the user see
// every running session across every registered city in one table
// and send a nudge to any of them via a single chat-style input.
//
// Why this exists:
//   The dashboard already has per-city panels (crew, mail, convoys,
//   issues), but they only render the city you've clicked into.
//   When you're running a meta-city ("command-center" style) that
//   watches multiple downstream cities, you need a single supervisor-
//   mode surface that shows what every agent is doing AND lets you
//   talk to any of them without switching tabs first.
//
// What slice 1a renders (this file):
//   - one row per running session across all cities, sortable by city
//     then session ID
//   - a chat input below the table that targets any session and POSTs
//     a nudge via the supervisor's typed /messages endpoint
//
// What slice 1b adds (next):
//   - live agent-output stream below the input so the chat reads as a
//     conversation rather than just a send box (cc-082.5 / cc-082.15)
//
// The panel is supervisor-scope-only — it hides itself when a city
// is selected, same gate as supervisor.ts.

import { cityAPI, cityScope, type SessionRecord } from "../api";
import { getCachedCities } from "../state";
import { byId, clear, el } from "../util/dom";
import { showToast } from "../ui";
import { logInfo, logWarn } from "../logger";

interface OversightSessionRow {
  cityName: string;
  cityRunning: boolean;
  session: SessionRecord;
}

let lastRows: OversightSessionRow[] = [];

export async function renderOversight(): Promise<void> {
  const panel = byId("oversight-panel");
  const sessions = byId("oversight-sessions");
  const count = byId("oversight-session-count");
  if (!panel || !sessions || !count) return;

  const inSupervisorMode = cityScope() === "";
  panel.hidden = !inSupervisorMode;
  if (!inSupervisorMode) return;

  const cities = getCachedCities()
    .filter((c) => c.running)
    .sort((a, b) => a.name.localeCompare(b.name));

  const fetched = await Promise.allSettled(
    cities.map(async (c) => {
      const { data, error } = await cityAPI(c.name).sessions({ peek: false });
      if (error || !data?.items) return [] as OversightSessionRow[];
      return data.items.map((session) => ({
        cityName: c.name,
        cityRunning: c.running,
        session,
      }));
    }),
  );

  const rows = fetched
    .flatMap((r) => (r.status === "fulfilled" ? r.value : []))
    .sort((a, b) => {
      if (a.cityName !== b.cityName) return a.cityName.localeCompare(b.cityName);
      return (a.session.id ?? "").localeCompare(b.session.id ?? "");
    });
  lastRows = rows;

  count.textContent = String(rows.length);
  clear(sessions);
  if (rows.length === 0) {
    sessions.append(el("div", { class: "panel-muted" }, [
      cities.length === 0
        ? "No running cities — start a city to see sessions here."
        : "No active sessions across the running cities.",
    ]));
    refreshTargetSelect();
    return;
  }

  const tbody = el("tbody");
  for (const row of rows) {
    const sessionId = row.session.id ?? "";
    const agentName = agentLabel(row.session);
    const stateLabel = row.session.state ?? "?";
    const beadId = row.session.active_bead ?? "";
    tbody.append(el(
      "tr",
      { class: `oversight-session-row state-${stateLabel}` },
      [
        el("td", { class: "oversight-city" }, [row.cityName]),
        el("td", { class: "oversight-agent" }, [agentName]),
        el("td", { class: "oversight-session-id" }, [sessionId]),
        el("td", {}, [el("span", { class: `badge badge-${badgeClassForState(stateLabel)}` }, [stateLabel])]),
        el("td", { class: "oversight-bead" }, [beadId || "—"]),
      ],
    ));
  }

  sessions.append(el("table", { class: "oversight-session-table" }, [
    el("thead", {}, [
      el("tr", {}, [
        el("th", {}, ["City"]),
        el("th", {}, ["Agent"]),
        el("th", {}, ["Session"]),
        el("th", {}, ["State"]),
        el("th", {}, ["Bead"]),
      ]),
    ]),
    tbody,
  ]));

  refreshTargetSelect();
}

// installOversightInteractions wires the chat-form once at boot. Form
// submit (button click or Cmd/Ctrl+Enter) reads the selected target
// and message, POSTs via cityAPI.sendMessage(), and surfaces success
// or failure as a toast. The handler is idempotent and safe to call
// multiple times — we tag the form with a data-attribute marker.
let chatFormInstalled = false;
export function installOversightInteractions(): void {
  if (chatFormInstalled) return;
  const form = byId<HTMLFormElement>("oversight-chat-form");
  const textarea = byId<HTMLTextAreaElement>("oversight-chat-input");
  const status = byId<HTMLSpanElement>("oversight-chat-status");
  if (!form || !textarea || !status) return;
  chatFormInstalled = true;

  textarea.addEventListener("keydown", (ev) => {
    const isSubmit = (ev.metaKey || ev.ctrlKey) && ev.key === "Enter";
    if (!isSubmit) return;
    ev.preventDefault();
    form.requestSubmit();
  });

  form.addEventListener("submit", (ev) => {
    ev.preventDefault();
    void submitChatMessage();
  });
}

async function submitChatMessage(): Promise<void> {
  const target = byId<HTMLSelectElement>("oversight-chat-target");
  const textarea = byId<HTMLTextAreaElement>("oversight-chat-input");
  const status = byId<HTMLSpanElement>("oversight-chat-status");
  const sendBtn = byId<HTMLButtonElement>("oversight-chat-send");
  if (!target || !textarea || !status || !sendBtn) return;

  const message = textarea.value.trim();
  if (message === "") {
    status.textContent = "Type a message first.";
    return;
  }
  const targetValue = target.value;
  if (!targetValue) {
    status.textContent = "Pick a session from the dropdown.";
    return;
  }
  const [cityName, sessionId] = decodeTarget(targetValue);
  if (!cityName || !sessionId) {
    status.textContent = "Selected target is not a valid city/session pair.";
    return;
  }

  sendBtn.disabled = true;
  status.textContent = "Sending…";
  logInfo("oversight", "Send nudge", { cityName, sessionId, length: message.length });

  try {
    const { data, error } = await cityAPI(cityName).sendMessage(sessionId, message);
    if (error) {
      const detail = errorDetail(error);
      logWarn("oversight", "Send nudge failed", { cityName, sessionId, detail });
      status.textContent = `Failed: ${detail}`;
      showToast("error", "Nudge failed", `${cityName}/${sessionId}: ${detail}`);
      return;
    }
    const okStatus = data?.status ?? "delivered";
    status.textContent = `Sent (${okStatus}).`;
    textarea.value = "";
    showToast("success", "Nudge sent", `${cityName}/${sessionId}`);
  } catch (err) {
    const detail = err instanceof Error ? err.message : String(err);
    logWarn("oversight", "Send nudge threw", { cityName, sessionId, detail });
    status.textContent = `Failed: ${detail}`;
    showToast("error", "Nudge failed", `${cityName}/${sessionId}: ${detail}`);
  } finally {
    sendBtn.disabled = false;
  }
}

// refreshTargetSelect rebuilds the chat-form's <select> options from
// the rows just rendered so the picker stays in sync with what the
// user can see in the table. Preserves the previous selection when
// the same city/session is still alive after a refresh.
function refreshTargetSelect(): void {
  const select = byId<HTMLSelectElement>("oversight-chat-target");
  if (!select) return;

  const previous = select.value;
  clear(select);

  if (lastRows.length === 0) {
    select.append(el("option", { value: "", disabled: true, selected: true }, ["No active sessions"]));
    select.disabled = true;
    return;
  }

  select.disabled = false;
  for (const row of lastRows) {
    const sessionId = row.session.id ?? "";
    if (sessionId === "") continue;
    const agent = agentLabel(row.session);
    const value = encodeTarget(row.cityName, sessionId);
    const option = el("option", { value }, [`${row.cityName}/${agent} (${sessionId})`]);
    select.append(option);
  }

  if (previous !== "" && Array.from(select.options).some((o) => o.value === previous)) {
    select.value = previous;
  }
}

function encodeTarget(cityName: string, sessionId: string): string {
  return `${cityName}\u0000${sessionId}`;
}

function decodeTarget(value: string): [string, string] {
  const idx = value.indexOf("\u0000");
  if (idx < 0) return ["", ""];
  return [value.slice(0, idx), value.slice(idx + 1)];
}

// agentLabel picks the most user-friendly identifier from a SessionResponse:
// display_name (UI label set by the operator), then template (the agent type
// like "polecat" / "overseer"), then alias (a short rename), then session_name
// (the long auto-generated name) as a last resort. Never returns empty —
// drives the dropdown labels and table cells.
function agentLabel(session: SessionRecord): string {
  return (
    session.display_name?.trim()
    || session.template?.trim()
    || session.alias?.trim()
    || session.session_name?.trim()
    || "agent"
  );
}

function badgeClassForState(state: string): string {
  switch (state) {
    case "active":
    case "running":
      return "green";
    case "asleep":
    case "sleeping":
      return "muted";
    case "creating":
    case "starting":
      return "blue";
    case "stopped":
    case "closed":
      return "red";
    default:
      return "muted";
  }
}

function errorDetail(error: unknown): string {
  if (typeof error === "string") return error;
  if (error && typeof error === "object" && "detail" in error) {
    const detail = (error as { detail?: unknown }).detail;
    if (typeof detail === "string") return detail;
  }
  return "request failed";
}
