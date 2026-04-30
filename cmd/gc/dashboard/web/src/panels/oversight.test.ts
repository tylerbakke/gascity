import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { api, type SessionRecord } from "../api";
import { setCachedCities, syncCityScopeFromLocation } from "../state";
import { installOversightInteractions, renderOversight } from "./oversight";

// Minimal toast-container stub so showToast() during the form-submit
// path doesn't throw a null-deref. Each test re-installs the DOM.
const PANEL_HTML = `
  <div id="oversight-panel" hidden>
    <div id="oversight-session-count">0</div>
    <div id="oversight-sessions"></div>
    <form id="oversight-chat-form">
      <select id="oversight-chat-target"></select>
      <textarea id="oversight-chat-input"></textarea>
      <span id="oversight-chat-status"></span>
      <button id="oversight-chat-send" type="submit"></button>
    </form>
  </div>
  <div id="toast-container"></div>
`;

function seedSession(overrides: Partial<{ id: string; template: string; state: string; active_bead: string }>): SessionRecord {
  return {
    attached: true,
    created_at: "2026-04-29T00:00:00Z",
    id: overrides.id ?? "ja-k52",
    provider: "claude",
    running: true,
    session_name: overrides.id ?? "ja-k52",
    state: overrides.state ?? "active",
    template: overrides.template ?? "polecat",
    title: "test session",
    active_bead: overrides.active_bead,
  };
}

describe("oversight panel", () => {
  beforeEach(() => {
    document.body.innerHTML = PANEL_HTML;
    window.history.pushState({}, "", "/dashboard");
    syncCityScopeFromLocation();
    setCachedCities([]);
  });

  afterEach(() => {
    vi.restoreAllMocks();
    setCachedCities([]);
  });

  it("hides itself when a city is scoped", async () => {
    setCachedCities([{ name: "trader", phasesCompleted: [], running: true }]);
    window.history.pushState({}, "", "/dashboard?city=trader");
    syncCityScopeFromLocation();
    await renderOversight();
    expect(document.getElementById("oversight-panel")?.hidden).toBe(true);
  });

  it("renders an empty-state message when no cities are running", async () => {
    setCachedCities([]);
    await renderOversight();
    const panel = document.getElementById("oversight-panel");
    expect(panel?.hidden).toBe(false);
    expect(document.getElementById("oversight-sessions")?.textContent).toContain("No running cities");
  });

  it("fans out per-city /sessions and renders one row per session", async () => {
    setCachedCities([
      { name: "jarvis", phasesCompleted: [], running: true },
      { name: "trader", phasesCompleted: [], running: true },
      { name: "stopped-city", phasesCompleted: [], running: false },
    ]);

    const get = vi.spyOn(api, "GET");
    get.mockImplementation(((path: string, opts: { params?: { path?: { cityName?: string } } } = {}) => {
      const cityName = opts.params?.path?.cityName ?? "";
      if (path === "/v0/city/{cityName}/sessions" && cityName === "jarvis") {
        return Promise.resolve({
          data: { items: [seedSession({ id: "gj-9f8s", template: "polecat", state: "active", active_bead: "ja-k52" })] },
        });
      }
      if (path === "/v0/city/{cityName}/sessions" && cityName === "trader") {
        return Promise.resolve({
          data: { items: [seedSession({ id: "gt-vyg8", template: "surveyor", state: "asleep" })] },
        });
      }
      return Promise.resolve({ data: { items: [] } });
    }) as never);

    await renderOversight();

    const sessionsHTML = document.getElementById("oversight-sessions")?.innerHTML ?? "";
    expect(sessionsHTML).toContain("jarvis");
    expect(sessionsHTML).toContain("trader");
    expect(sessionsHTML).toContain("polecat");
    expect(sessionsHTML).toContain("gj-9f8s");
    expect(sessionsHTML).toContain("ja-k52");
    expect(document.getElementById("oversight-session-count")?.textContent).toBe("2");

    // The stopped city must be filtered out before any /sessions call —
    // its session would appear if filtering broke.
    expect(sessionsHTML).not.toContain("stopped-city");

    // Dropdown is populated from rendered rows so the user can target
    // any visible session immediately.
    const targets = Array.from(document.querySelectorAll<HTMLOptionElement>("#oversight-chat-target option"));
    const targetLabels = targets.map((o) => o.textContent ?? "");
    expect(targetLabels.some((l) => l.includes("jarvis/polecat") && l.includes("gj-9f8s"))).toBe(true);
    expect(targetLabels.some((l) => l.includes("trader/surveyor") && l.includes("gt-vyg8"))).toBe(true);
  });

  it("posts the message to the supervisor and clears the textarea on success", async () => {
    setCachedCities([{ name: "jarvis", phasesCompleted: [], running: true }]);

    const get = vi.spyOn(api, "GET");
    get.mockResolvedValue({
      data: { items: [seedSession({ id: "gj-9f8s", template: "polecat" })] },
    } as never);

    await renderOversight();
    installOversightInteractions();

    const post = vi.spyOn(api, "POST");
    post.mockResolvedValue({ data: { id: "gj-9f8s", status: "delivered" } } as never);

    const textarea = document.getElementById("oversight-chat-input") as HTMLTextAreaElement;
    textarea.value = "hello agent";
    const form = document.getElementById("oversight-chat-form") as HTMLFormElement;
    form.requestSubmit();

    // requestSubmit() invokes the listener synchronously, but the
    // handler itself is async. Wait for the microtask queue to drain
    // and the POST + UI update to settle.
    await new Promise((r) => setTimeout(r, 0));
    await new Promise((r) => setTimeout(r, 0));

    expect(post).toHaveBeenCalledTimes(1);
    const [calledPath, calledOpts] = post.mock.calls[0] as [string, { body: { message: string }; params: { path: { cityName: string; id: string } } }];
    expect(calledPath).toBe("/v0/city/{cityName}/session/{id}/messages");
    expect(calledOpts.body.message).toBe("hello agent");
    expect(calledOpts.params.path.cityName).toBe("jarvis");
    expect(calledOpts.params.path.id).toBe("gj-9f8s");
    expect(textarea.value).toBe("");
    expect(document.getElementById("oversight-chat-status")?.textContent).toContain("Sent");
  });
});
