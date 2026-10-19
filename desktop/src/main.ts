import "./styles/base.css";
import { confirmRemove, openInstanceForm } from "./components/instance-form";
import { renderSidebar } from "./components/sidebar";
import {
  activateTab,
  addInstance,
  closeTab,
  listInstances,
  listTabs,
  onInstancesChanged,
  onTabState,
  onTabTitle,
  onTabsChanged,
  onToggleSidebar,
  openTab,
  removeInstance,
  reorderInstances,
  setContentVisible,
  setStageBounds,
  updateInstance,
} from "./ipc";
import type { Instance, TabInfo } from "./types";

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
          <h2>Agent Hub</h2>
          <p>Pick an instance on the left to open its page in a tab.</p>
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

let instances: Instance[] = [];
let tabs: TabInfo[] = [];
let stageKey = "";

function showError(error: unknown): void {
  errorBar.textContent = error instanceof Error ? error.message : String(error);
  errorBar.hidden = false;
}

function clearError(): void {
  errorBar.hidden = true;
}

function renderInstances(): void {
  renderSidebar(sidebar, instances, {
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
  });
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
    }`;
    item.dataset.id = tab.instance_id;

    const label = document.createElement("button");
    label.type = "button";
    label.className = "tab-label";
    label.title = tab.url;
    label.textContent = tab.title.trim() === "" ? tab.instance_name : tab.title;
    label.addEventListener("click", () => {
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

    item.append(label, close);
    tabsEl.append(item);
  }
}

async function refreshInstances(): Promise<void> {
  instances = await listInstances();
  clearError();
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
  renderTabs();
}

async function refreshTabs(): Promise<void> {
  applyTabs(await listTabs());
}

async function syncStageBounds(): Promise<void> {
  const rect = stage.getBoundingClientRect();
  const key = `${rect.x}:${rect.y}:${rect.width}:${rect.height}`;
  if (key === stageKey) return;
  stageKey = key;
  try {
    await setStageBounds(rect.x, rect.y, rect.width, rect.height);
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
  if (tab.active) return;
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
  const input = await withHiddenContent(() => openInstanceForm());
  if (input === null) return;
  try {
    await addInstance(input);
    await refreshInstances();
    await refreshTabs();
  } catch (error) {
    showError(error);
  }
}

async function edit(instance: Instance): Promise<void> {
  const input = await withHiddenContent(() => openInstanceForm(instance));
  if (input === null) return;
  try {
    await updateInstance(instance.id, input);
    await refreshInstances();
    await refreshTabs();
  } catch (error) {
    showError(error);
  }
}

async function remove(instance: Instance): Promise<void> {
  const confirmed = await withHiddenContent(() =>
    confirmRemove(instance.name),
  );
  if (!confirmed) return;
  try {
    await removeInstance(instance.id);
    await refreshInstances();
    await refreshTabs();
  } catch (error) {
    showError(error);
  }
}

async function move(instance: Instance, direction: -1 | 1): Promise<void> {
  const index = instances.findIndex((candidate) => candidate.id === instance.id);
  const target = index + direction;
  if (index < 0 || target < 0 || target >= instances.length) return;

  const ids = instances.map((candidate) => candidate.id);
  [ids[index], ids[target]] = [ids[target], ids[index]];
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
  layout.classList.toggle("sidebar-hidden");
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

void onInstancesChanged((next) => {
  instances = next;
  clearError();
  renderInstances();
});

void onTabsChanged((next) => {
  applyTabs(next);
});

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

async function init(): Promise<void> {
  try {
    await refreshInstances();
    await refreshTabs();
    await syncStageBounds();
  } catch (error) {
    showError(error);
  }
}

void init();
