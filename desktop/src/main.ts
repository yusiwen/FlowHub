import "./styles/base.css";
import { confirmRemove, openInstanceForm } from "./components/instance-form";
import { renderSidebar } from "./components/sidebar";
import {
  activateTab,
  addInstance,
  closeTab,
  hubSnapshot,
  listHealth,
  listInstances,
  loadSession,
  listTabs,
  onAuthFailed,
  onAuthOk,
  onAuthRequired,
  onHealthChanged,
  onHubReload,
  onInstancesChanged,
  onNewTab,
  onScopeSelect,
  onTabState,
  onTabTitle,
  onTabsChanged,
  onToggleSidebar,
  openTab,
  probeAll,
  removeInstance,
  reorderInstances,
  setContentVisible,
  setSidebarVisible,
  setStageBounds,
  submitToken,
  updateInstance,
} from "./ipc";
import { roleOf } from "./types";
import type {
  HealthReport,
  HubSnapshot,
  Instance,
  Role,
  TabInfo,
} from "./types";

const app = document.querySelector<HTMLDivElement>("#app");
if (app === null) throw new Error("shell: missing #app");

app.innerHTML = `
  <div class="layout">
    <aside class="sidebar" id="sidebar"></aside>
    <section class="workspace">
      <div class="tabbar">
        <button type="button" class="icon-button" id="sidebar-toggle" title="Toggle sidebar (Cmd+B)">☰</button>
        <div class="tabs" id="tabs"></div>
      </div>
      <div class="stage" id="stage">
        <div class="stage-empty" id="stage-empty">
          <h2>FlowHub</h2>
          <p>Pick a page on the left to open it in a tab.</p>
        </div>
        <section class="hub-panel" id="hub-panel" hidden>
          <header class="hub-header">
            <div class="hub-identity">
              <h2 id="hub-name"></h2>
              <p class="hub-url" id="hub-url"></p>
            </div>
            <div class="hub-actions">
              <span class="health-dot unknown" id="hub-dot"></span>
              <span class="hub-status" id="hub-status">Not connected</span>
              <button type="button" class="ghost-button" id="hub-refresh">Refresh</button>
            </div>
          </header>
          <p class="hub-warning" id="hub-warning" hidden></p>
          <p class="hub-error" id="hub-error" role="alert" hidden></p>
          <div class="hub-grid" id="hub-cards"></div>
        </section>
        <div class="auth-panel" id="auth-panel" hidden>
          <form class="auth-card" id="auth-form">
            <h2>Sign in to <span id="auth-name"></span></h2>
            <p class="auth-hint">Paste the URL printed by the <code>dsh web</code> command. The token is exchanged for a cookie and is not stored.</p>
            <input id="auth-input" autocomplete="off" spellcheck="false" placeholder="http://127.0.0.1:3080/?token=..." />
            <p class="form-error" id="auth-error" role="alert" hidden></p>
            <div class="dialog-actions">
              <button type="button" class="ghost-button" id="auth-cancel">Cancel</button>
              <button type="submit" class="primary-button" id="auth-submit">Sign in</button>
            </div>
          </form>
        </div>
      </div>
    </section>
  </div>
  <p class="error" id="error" role="alert" hidden></p>
`;

const sidebar = document.querySelector<HTMLElement>("#sidebar")!;
const tabsEl = document.querySelector<HTMLElement>("#tabs")!;
const stage = document.querySelector<HTMLElement>("#stage")!;
const layout = document.querySelector<HTMLElement>(".layout")!;
const stageEmpty = document.querySelector<HTMLElement>("#stage-empty")!;
const errorBar = document.querySelector<HTMLElement>("#error")!;
const authPanel = document.querySelector<HTMLElement>("#auth-panel")!;
const authForm = document.querySelector<HTMLFormElement>("#auth-form")!;
const authName = document.querySelector<HTMLElement>("#auth-name")!;
const authInput = document.querySelector<HTMLInputElement>("#auth-input")!;
const authError = document.querySelector<HTMLElement>("#auth-error")!;
const authSubmit = document.querySelector<HTMLButtonElement>("#auth-submit")!;
const authCancel = document.querySelector<HTMLButtonElement>("#auth-cancel")!;
const hubPanel = document.querySelector<HTMLElement>("#hub-panel")!;
const hubName = document.querySelector<HTMLElement>("#hub-name")!;
const hubUrlEl = document.querySelector<HTMLElement>("#hub-url")!;
const hubDot = document.querySelector<HTMLElement>("#hub-dot")!;
const hubStatus = document.querySelector<HTMLElement>("#hub-status")!;
const hubWarning = document.querySelector<HTMLElement>("#hub-warning")!;
const hubError = document.querySelector<HTMLElement>("#hub-error")!;
const hubCards = document.querySelector<HTMLElement>("#hub-cards")!;
const hubRefresh = document.querySelector<HTMLButtonElement>("#hub-refresh")!;

let instances: Instance[] = [];
let tabs: TabInfo[] = [];
const health = new Map<string, HealthReport>();
const lastActive = new Map<Role, string>();
let scope: Role = "downstream";
let stageKey = "";
let authInstanceId: string | null = null;
let hubTabId: string | null = null;
let hubUrl = "";
let hubSnapshotValue: HubSnapshot | null = null;
let hubUpdatedAt = 0;
let hubFailed = false;
let hubLoading = false;
let hubRequest = 0;
let hubTimer: number | null = null;

function showError(error: unknown): void {
  errorBar.textContent = error instanceof Error ? error.message : String(error);
  errorBar.hidden = false;
}

function clearError(): void {
  errorBar.hidden = true;
}

function showAuth(instanceId: string, name: string): void {
  authInstanceId = instanceId;
  authName.textContent = name;
  authInput.value = "";
  authError.hidden = true;
  authSubmit.disabled = false;
  authPanel.hidden = false;
  requestAnimationFrame(() => authInput.focus());
}

function hideAuth(): void {
  authPanel.hidden = true;
  authInstanceId = null;
}

function scopedInstances(): Instance[] {
  return instances.filter((instance) => roleOf(instance.kind) === scope);
}

function renderInstances(): void {
  renderSidebar(sidebar, scope, scopedInstances(), health, {
    onAdd: () => {
      void add();
    },
    onEdit: (instance) => {
      void edit(instance);
    },
    onRemove: (instance) => {
      void remove(instance);
    },
    onMove: (instance, direction) => {
      void move(instance, direction);
    },
    onOpen: (instance) => {
      void open(instance);
    },
    onScope: (next) => {
      switchScope(next);
    },
  });
}

function tabLabelParts(tab: TabInfo): { title: string; suffix: string } {
  const title = tab.title.trim();
  return {
    title,
    // The suffix always ends the label; the leading space is preserved by the
    // suffix element's `white-space: pre`.
    suffix: title === "" ? `[${tab.instance_name}]` : ` [${tab.instance_name}]`,
  };
}

function tabTooltip(tab: TabInfo): string {
  // Built from the untruncated values: the visible label ellipsizes the title,
  // so the tooltip is the only place the full title remains readable.
  return [tab.title.trim(), tab.instance_name, tab.url]
    .filter((line) => line !== "")
    .join("\n");
}

function renderTabs(): void {
  tabsEl.replaceChildren();

  if (tabs.length === 0) {
    const hint = document.createElement("span");
    hint.className = "tabbar-empty";
    hint.textContent = "No tab open";
    tabsEl.append(hint);
    stageEmpty.hidden = false;
    return;
  }

  stageEmpty.hidden = true;

  for (const tab of tabs) {
    const item = document.createElement("div");
    item.className = `tab${tab.active ? " active" : ""}${
      tab.loading ? " loading" : ""
    }${tab.loaded ? "" : " placeholder"}`;
    item.dataset.id = tab.instance_id;

    const label = document.createElement("button");
    label.type = "button";
    label.className = "tab-label";
    label.title = tabTooltip(tab);
    const parts = tabLabelParts(tab);
    if (parts.title !== "") {
      const title = document.createElement("span");
      title.className = "tab-title";
      title.textContent = parts.title;
      label.append(title);
    }
    const suffix = document.createElement("span");
    suffix.className = "tab-instance";
    suffix.textContent = parts.suffix;
    label.append(suffix);
    label.addEventListener("click", () => {
      if (tab.auth_required) showAuth(tab.instance_id, tab.instance_name);
      void activate(tab);
    });

    const close = document.createElement("button");
    close.type = "button";
    close.className = "tab-close";
    close.title = "Close tab";
    close.textContent = "×";
    close.addEventListener("click", (event) => {
      event.stopPropagation();
      void closeOne(tab);
    });

    item.append(label);
    if (tab.auth_required) {
      const lock = document.createElement("span");
      lock.className = "tab-lock";
      lock.textContent = "sign in";
      lock.title = "Sign in required";
      item.append(lock);
    }
    item.append(close);
    tabsEl.append(item);
  }
}

async function refreshInstances(): Promise<void> {
  instances = await listInstances();
  clearError();
  renderInstances();
  syncHubPanel();
}

function applyHealth(reports: HealthReport[]): void {
  for (const report of reports) {
    health.set(report.instance_id, report);
  }
  renderInstances();
}

function applyTabs(next: TabInfo[]): void {
  const previous = new Map(tabs.map((tab) => [tab.instance_id, tab]));
  tabs = next.map((tab) => {
    const old = previous.get(tab.instance_id);
    if (old === undefined) return tab;
    // The backend list carries the instance defaults; the page title and the
    // loading flag arrive as events and must survive a list refresh.
    return { ...tab, title: old.title, url: old.url, loading: old.loading };
  });
  const active = tabs.find((tab) => tab.active);
  if (active !== undefined) {
    lastActive.set(active.role, active.instance_id);
    if (active.role !== scope) {
      scope = active.role;
      renderInstances();
    }
  }
  if (
    authInstanceId !== null &&
    active !== undefined &&
    (active.instance_id !== authInstanceId || !active.auth_required)
  ) {
    hideAuth();
  }
  if (authInstanceId !== null && !tabs.some((tab) => tab.instance_id === authInstanceId)) {
    hideAuth();
  }
  renderTabs();
  syncHubPanel();
}

async function refreshTabs(): Promise<void> {
  applyTabs(await listTabs());
}

async function syncStageBounds(): Promise<void> {
  const rect = stage.getBoundingClientRect();
  // The shell viewport can be shorter than the window content view: WebKit
  // insets it below the title bar. Rust needs the viewport height to translate
  // the stage into content-view coordinates.
  const viewportHeight = window.innerHeight;
  const key = `${rect.x}:${rect.y}:${rect.width}:${rect.height}:${viewportHeight}`;
  if (key === stageKey) return;
  stageKey = key;
  try {
    await setStageBounds(
      rect.x,
      rect.y,
      rect.width,
      rect.height,
      viewportHeight,
    );
  } catch (error) {
    stageKey = "";
    throw error;
  }
}

async function withHiddenContent<T>(action: () => Promise<T>): Promise<T> {
  await setContentVisible(false).catch(() => undefined);
  try {
    return await action();
  } finally {
    await setContentVisible(true).catch(() => undefined);
  }
}

async function open(instance: Instance): Promise<void> {
  try {
    await syncStageBounds();
    await openTab(instance.id);
    await refreshTabs();
  } catch (error) {
    showError(error);
  }
}

async function activate(tab: TabInfo): Promise<void> {
  if (tab.active && tab.loaded) return;
  try {
    await activateTab(tab.instance_id);
    await refreshTabs();
  } catch (error) {
    showError(error);
  }
}

async function closeOne(tab: TabInfo): Promise<void> {
  try {
    await closeTab(tab.instance_id);
    await refreshTabs();
  } catch (error) {
    showError(error);
  }
}

async function add(): Promise<void> {
  hideAuth();
  const input = await withHiddenContent(() => openInstanceForm(scope));
  if (input === null) return;
  try {
    await addInstance(input);
    await refreshInstances();
    await refreshTabs();
    void probeAll().then(applyHealth).catch(showError);
  } catch (error) {
    showError(error);
  }
}

async function edit(instance: Instance): Promise<void> {
  hideAuth();
  const input = await withHiddenContent(() =>
    openInstanceForm(scope, instance),
  );
  if (input === null) return;
  try {
    await updateInstance(instance.id, input);
    await refreshInstances();
    await refreshTabs();
    void probeAll().then(applyHealth).catch(showError);
  } catch (error) {
    showError(error);
  }
}

async function remove(instance: Instance): Promise<void> {
  hideAuth();
  const confirmed = await withHiddenContent(() =>
    confirmRemove(instance.name, scope),
  );
  if (!confirmed) return;
  try {
    await removeInstance(instance.id);
    health.delete(instance.id);
    await refreshInstances();
    await refreshTabs();
  } catch (error) {
    showError(error);
  }
}

async function move(instance: Instance, direction: -1 | 1): Promise<void> {
  // The registry order is global; a scope only decides which neighbours are
  // visible. Swapping the two visible rows in the full id list keeps every
  // other row where it was.
  const visible = scopedInstances();
  const index = visible.findIndex((candidate) => candidate.id === instance.id);
  const target = index + direction;
  if (index < 0 || target < 0 || target >= visible.length) return;

  const ids = instances.map((candidate) => candidate.id);
  const from = ids.indexOf(visible[index].id);
  const to = ids.indexOf(visible[target].id);
  if (from < 0 || to < 0) return;
  [ids[from], ids[to]] = [ids[to], ids[from]];
  try {
    await reorderInstances(ids);
    await refreshInstances();
  } catch (error) {
    showError(error);
  }
}

const observer = new ResizeObserver(() => {
  void syncStageBounds().catch(showError);
});
observer.observe(stage);
window.addEventListener("resize", () => {
  void syncStageBounds().catch(showError);
});

function toggleSidebar(): void {
  const hidden = layout.classList.toggle("sidebar-hidden");
  void setSidebarVisible(!hidden).catch(showError);
  requestAnimationFrame(() => {
    void syncStageBounds().catch(showError);
  });
}

document
  .querySelector("#sidebar-toggle")
  ?.addEventListener("click", toggleSidebar);

void onToggleSidebar(() => {
  toggleSidebar();
});

void onNewTab(() => {
  openNext();
});

void onScopeSelect((next) => {
  if (next === "upstream" || next === "downstream" || next === "hub") {
    switchScope(next);
  }
});

void onHubReload(() => {
  void fetchHub();
});

hubRefresh.addEventListener("click", () => {
  void fetchHub();
});

void onHealthChanged((report) => {
  health.set(report.instance_id, report);
  renderInstances();
});

void onInstancesChanged((next) => {
  instances = next;
  clearError();
  renderInstances();
});

void onTabsChanged((next) => {
  applyTabs(next);
});

void onAuthRequired((event) => {
  showAuth(event.instance_id, event.instance_name);
  void refreshTabs();
});

void onAuthOk(() => {
  hideAuth();
  void refreshTabs();
});

void onAuthFailed((event) => {
  if (authInstanceId !== event.instance_id) return;
  authError.textContent = event.reason;
  authError.hidden = false;
  authSubmit.disabled = false;
});

authForm.addEventListener("submit", (event) => {
  event.preventDefault();
  const id = authInstanceId;
  if (id === null) return;
  const input = authInput.value.trim();
  if (input === "") {
    authError.textContent = "Paste the URL printed by dsh web.";
    authError.hidden = false;
    return;
  }
  authError.hidden = true;
  authSubmit.disabled = true;
  submitToken(id, input).catch((error) => {
    authError.textContent =
      error instanceof Error ? error.message : String(error);
    authError.hidden = false;
    authSubmit.disabled = false;
  });
});

authCancel.addEventListener("click", hideAuth);

void onTabTitle((event) => {
  const tab = tabs.find(
    (candidate) => candidate.instance_id === event.instance_id,
  );
  if (tab === undefined) return;
  tab.title = event.title;
  renderTabs();
});

void onTabState((event) => {
  const tab = tabs.find(
    (candidate) => candidate.instance_id === event.instance_id,
  );
  if (tab === undefined) return;
  tab.url = event.url;
  tab.loading = event.loading;
  renderTabs();
});

/// A scope switch activates the page this view was last on. A view with no open
/// page only changes the list: blanking the stage for it would hide the page the
/// user is reading, and the tab strip would still name it as active.
function switchScope(next: Role): void {
  if (next === scope) {
    renderInstances();
    return;
  }
  scope = next;
  renderInstances();
  const remembered = lastActive.get(next);
  if (remembered === undefined) return;
  const tab = tabs.find((candidate) => candidate.instance_id === remembered);
  if (tab === undefined) {
    lastActive.delete(next);
    return;
  }
  void activate(tab);
}

/// Cmd+T: the first row of this view without a tab; when every row is open,
/// cycle through this view's tabs instead of opening a duplicate.
function openNext(): void {
  const rows = scopedInstances();
  const openIds = new Set(tabs.map((tab) => tab.instance_id));
  const next = rows.find((row) => !openIds.has(row.id));
  if (next !== undefined) {
    void open(next);
    return;
  }
  const visible = tabs.filter((tab) => tab.role === scope);
  if (visible.length === 0) return;
  const active = tabs.find((tab) => tab.active);
  const index = visible.findIndex(
    (tab) => active !== undefined && tab.instance_id === active.instance_id,
  );
  void activate(visible[(index + 1) % visible.length]);
}

function formatUptime(seconds: number): string {
  if (seconds <= 0) return "—";
  const days = Math.floor(seconds / 86400);
  const hours = Math.floor((seconds % 86400) / 3600);
  const minutes = Math.floor((seconds % 3600) / 60);
  if (days > 0) return `${days}d ${hours}h`;
  if (hours > 0) return `${hours}h ${minutes}m`;
  return `${minutes}m ${seconds % 60}s`;
}

function hubCard(title: string): { card: HTMLElement; body: HTMLElement } {
  const card = document.createElement("section");
  card.className = "hub-card";
  const heading = document.createElement("h3");
  heading.textContent = title;
  const body = document.createElement("div");
  body.className = "hub-rows";
  card.append(heading, body);
  return { card, body };
}

function hubRow(
  body: HTMLElement,
  label: string,
  value: string,
  mono = false,
): void {
  const row = document.createElement("div");
  row.className = "hub-row";
  const key = document.createElement("span");
  key.className = "hub-key";
  key.textContent = label;
  const text = document.createElement("span");
  text.className = mono ? "hub-value hub-mono" : "hub-value";
  text.textContent = value;
  row.append(key, text);
  body.append(row);
}

/// Paths are too long for the right-aligned column; put the value under the
/// label so it stays readable instead of breaking mid-segment.
function hubPathRow(body: HTMLElement, label: string, value: string): void {
  if (value === "") {
    hubRow(body, label, "—");
    return;
  }
  const row = document.createElement("div");
  row.className = "hub-row hub-row-block";
  const key = document.createElement("span");
  key.className = "hub-key";
  key.textContent = label;
  const text = document.createElement("span");
  text.className = "hub-value hub-mono";
  text.textContent = value || "—";
  row.append(key, text);
  body.append(row);
}

function hubNumber(value: number | undefined): string {
  return value === undefined ? "—" : String(value);
}

function hubMap(value: Record<string, unknown> | undefined): string {
  if (value === undefined) return "—";
  const entries = Object.entries(value);
  if (entries.length === 0) return "—";
  return entries.map(([key, item]) => `${key} ${String(item)}`).join(" · ");
}

function renderHub(): void {
  const snapshot = hubSnapshotValue;
  const report = hubTabId === null ? undefined : health.get(hubTabId);
  const healthy = snapshot !== null && !hubFailed;
  hubDot.className = `health-dot ${healthy ? "ok" : hubFailed ? "unreachable" : report?.status ?? "unknown"}`;
  hubDot.title = healthy
    ? "The dashboard's last /healthz read succeeded"
    : "The dashboard has no successful /healthz read yet";
  hubWarning.hidden = snapshot === null || !snapshot.listens_on_all_interfaces;
  hubWarning.textContent =
    "This service listens on every interface: the webhook entry is reachable from the network.";
  hubCards.replaceChildren();

  if (snapshot === null) {
    hubStatus.textContent = "Not connected";
    return;
  }
  const age = `${Math.max(0, Math.round((Date.now() - hubUpdatedAt) / 1000))}s ago`;
  hubStatus.textContent = hubFailed
    ? `Last good read ${age}`
    : `Connected · updated ${age}`;

  const service = hubCard("Service");
  hubRow(service.body, "Version", snapshot.version || "—");
  hubRow(service.body, "Uptime", formatUptime(snapshot.uptime_seconds));
  hubRow(service.body, "Address", snapshot.addr || "—");
  hubPathRow(service.body, "Data dir", snapshot.data_dir);
  hubCards.append(service.card);

  const dispatch = snapshot.dispatch;
  if (dispatch !== null) {
    const card = hubCard("Dispatch");
    const enabled = dispatch.enabled !== false;
    hubRow(card.body, "Enabled", enabled ? "yes" : "no");
    if (enabled) {
      hubRow(card.body, "Paused", dispatch.paused ? "yes (pause file present)" : "no");
      hubRow(card.body, "Agent", dispatch.agent || "—");
      hubRow(card.body, "Queued", hubNumber(dispatch.queued));
      hubRow(card.body, "Handled", hubNumber(dispatch.handled));
      hubRow(card.body, "Ignored", hubNumber(dispatch.ignored));
      hubRow(card.body, "Dropped", hubNumber(dispatch.dropped));
      for (const [name, runtime] of Object.entries(dispatch.runtimes ?? {})) {
        const parts = [
          runtime.pending === undefined ? "" : `${runtime.pending} queued`,
          runtime.in_flight === undefined ? "" : `${runtime.in_flight} in flight`,
          runtime.breadth === undefined ? "" : `breadth ${runtime.breadth}`,
        ].filter((part) => part !== "");
        hubRow(card.body, `Runtime ${name}`, parts.join(" · ") || "—");
        if (runtime.last_error !== undefined && runtime.last_error !== "") {
          hubRow(card.body, `Last error (${name})`, runtime.last_error);
        }
      }
    }
    hubCards.append(card.card);
  }

  const runtimes = snapshot.runtimes;
  if (runtimes !== null) {
    const card = hubCard("Runtimes");
    if (runtimes.enabled === false) {
      hubRow(card.body, "Inventory", "disabled");
    } else {
      hubRow(card.body, "Counts", hubMap(runtimes.counts));
      for (const [name, state] of Object.entries(runtimes.states ?? {})) {
        hubRow(card.body, name, state);
      }
      hubPathRow(card.body, "File", runtimes.file || "");
    }
    hubCards.append(card.card);
  }

  const projects = snapshot.projects;
  if (projects !== null) {
    const card = hubCard("Projects");
    hubRow(card.body, "Mappings", hubNumber(projects.mappings));
    hubRow(card.body, "Routable", hubNumber(projects.routable));
    hubRow(card.body, "Keys", projects.keys?.join(", ") || "—");
    hubPathRow(card.body, "File", projects.file || "");
    hubCards.append(card.card);
  }

  const queues = hubCard("Queues");
  hubRow(
    queues.body,
    "Audit queue",
    `${snapshot.queue_depth} / ${snapshot.queue_capacity}`,
  );
  hubRow(queues.body, "Dedupe keys", String(snapshot.dedupe_keys));
  hubCards.append(queues.card);

  const counters = snapshot.counters;
  if (counters !== null) {
    const card = hubCard("Counters");
    hubRow(card.body, "Received", hubNumber(counters.received));
    hubRow(card.body, "Accepted", hubNumber(counters.accepted));
    hubRow(card.body, "Rejected", hubNumber(counters.rejected));
    hubRow(card.body, "Duplicates", hubNumber(counters.duplicates));
    hubRow(card.body, "Persisted", hubNumber(counters.persisted));
    hubRow(card.body, "Persist errors", hubNumber(counters.persist_errors));
    hubRow(card.body, "Reject reasons", hubMap(counters.reject_reasons));
    hubCards.append(card.card);
  }

  const paths = hubCard("Paths");
  hubPathRow(paths.body, "Audit", snapshot.audit_file);
  hubPathRow(paths.body, "Payload log", snapshot.payload_log);
  hubPathRow(paths.body, "App log", snapshot.app_log);
  hubCards.append(paths.card);
}

async function fetchHub(): Promise<void> {
  const id = hubTabId;
  if (id === null || hubLoading) return;
  const sequence = ++hubRequest;
  hubLoading = true;
  try {
    const snapshot = await hubSnapshot(id);
    if (sequence !== hubRequest || hubTabId !== id) return;
    hubSnapshotValue = snapshot;
    hubUpdatedAt = Date.now();
    hubFailed = false;
    hubError.hidden = true;
    renderHub();
  } catch (error) {
    if (sequence !== hubRequest || hubTabId !== id) return;
    hubFailed = true;
    hubError.textContent = error instanceof Error ? error.message : String(error);
    hubError.hidden = false;
    renderHub();
  } finally {
    if (sequence === hubRequest) hubLoading = false;
  }
}

function startHubTimer(): void {
  if (hubTimer !== null) return;
  hubTimer = window.setInterval(() => {
    void fetchHub();
  }, 5000);
}

function stopHubTimer(): void {
  if (hubTimer === null) return;
  window.clearInterval(hubTimer);
  hubTimer = null;
}

function showHub(tab: TabInfo): void {
  const instance = instances.find((candidate) => candidate.id === tab.instance_id);
  const url = instance?.url ?? tab.url;
  const name = instance?.name ?? tab.instance_name;
  const changed = hubTabId !== tab.instance_id || hubUrl !== url;
  hubTabId = tab.instance_id;
  hubUrl = url;
  hubName.textContent = name;
  hubUrlEl.textContent = url;
  hubPanel.hidden = false;
  startHubTimer();
  if (!changed) return;

  hubSnapshotValue = null;
  hubUpdatedAt = 0;
  hubFailed = false;
  hubRequest += 1;
  hubLoading = false;
  hubError.hidden = true;
  hubWarning.hidden = true;
  hubStatus.textContent = "Connecting…";
  hubDot.className = "health-dot unknown";
  hubCards.replaceChildren();
  void fetchHub();
}

function hideHub(): void {
  hubPanel.hidden = true;
  stopHubTimer();
  hubTabId = null;
  hubUrl = "";
  hubSnapshotValue = null;
  hubRequest += 1;
  hubLoading = false;
}

/// The dashboard follows the active tab, not the sidebar scope: switching to a
/// scope with no page leaves the stage where it is.
function syncHubPanel(): void {
  const active = tabs.find((tab) => tab.active);
  if (active !== undefined && active.role === "hub") {
    showHub(active);
  } else {
    hideHub();
  }
}

async function init(): Promise<void> {
  try {
    const session = await loadSession();
    if (!session.sidebar_visible) {
      layout.classList.add("sidebar-hidden");
    }
    await refreshInstances();
    applyHealth(await listHealth());
    await refreshTabs();
    await syncStageBounds();

    const active = tabs.find((tab) => tab.active);
    if (active !== undefined && !active.loaded) {
      await activateTab(active.instance_id);
      await refreshTabs();
    }
  } catch (error) {
    showError(error);
  }
  void probeAll().then(applyHealth).catch(showError);
}

void init();
