import type { HealthReport, Instance } from "../types";
import { kindLabel } from "../types";
import type { Role } from "../types";

export interface SidebarActions {
  onAdd: () => void;
  onEdit: (instance: Instance) => void;
  onRemove: (instance: Instance) => void;
  onMove: (instance: Instance, direction: -1 | 1) => void;
  onOpen: (instance: Instance) => void;
  onScope: (scope: Role) => void;
}

const SCOPES: ReadonlyArray<{ value: Role; label: string; shortcut: number }> = [
  { value: "upstream", label: "Upstream", shortcut: 1 },
  { value: "downstream", label: "Instances", shortcut: 2 },
  { value: "hub", label: "Hub", shortcut: 3 },
];

function countLabel(scope: Role, count: number): string {
  const noun = scope === "upstream" ? "upstream" : scope === "hub" ? "hub" : "instance";
  return `${count} ${noun}${count === 1 ? "" : "s"}`;
}

function emptyLabel(scope: Role): string {
  switch (scope) {
    case "upstream":
      return "No upstream yet. Add the YouTrack URL you work from.";
    case "hub":
      return "No Hub yet. Add your FlowHub service address (for example http://127.0.0.1:8080/).";
    default:
      return "No instances yet. Add the dsh web and opencode URLs you use.";
  }
}

function button(label: string, action: string, title: string): HTMLButtonElement {
  const element = document.createElement("button");
  element.type = "button";
  element.className = "icon-button";
  element.dataset.action = action;
  element.title = title;
  element.textContent = label;
  return element;
}

function statusLabel(report: HealthReport, kind: Instance["kind"]): string {
  switch (report.status) {
    case "ok":
      return kind === "dsh-web" ? "reachable, signed in" : "reachable";
    case "auth":
      if (kind !== "dsh-web") return "reachable, sign-in required";
      // A dsh 401 either means the app has no session yet (no cookie to send) or
      // that the server rejected the one the tab still holds.
      return report.credential === "cookie"
        ? "reachable, session expired — open the tab to sign in"
        : "reachable, sign-in required — open the tab";
    case "http":
      return "reachable, HTTP error";
    case "unreachable":
      return "unreachable";
  }
}

function row(
  instance: Instance,
  index: number,
  total: number,
  health: Map<string, HealthReport>,
  activeId: string | null,
): HTMLLIElement {
  const item = document.createElement("li");
  // The row backing the active tab owns the 3px marker; every other row is
  // plain, so the list and the strip always name the same object.
  item.className = instance.id === activeId ? "instance active" : "instance";
  item.dataset.id = instance.id;
  item.tabIndex = 0;
  item.title = "Open in a tab";

  const main = document.createElement("div");
  main.className = "instance-main";

  const heading = document.createElement("div");
  heading.className = "instance-heading";

  const name = document.createElement("span");
  name.className = "instance-name";
  name.textContent = instance.name;

  const badge = document.createElement("span");
  badge.className = "badge";
  badge.textContent = kindLabel(instance.kind);

  const report = health.get(instance.id);
  const dot = document.createElement("span");
  dot.className = `health-dot ${report?.status ?? "unknown"}`;
  dot.title =
    report === undefined
      ? "Not checked yet"
      : `${statusLabel(report, instance.kind)} · ${report.latency_ms} ms`;

  heading.append(dot, name, badge);

  const url = document.createElement("span");
  url.className = "instance-url";
  url.textContent = instance.url;

  main.append(heading, url);

  if (instance.group !== null) {
    const group = document.createElement("span");
    group.className = "instance-group";
    group.textContent = instance.group;
    main.append(group);
  }

  const actions = document.createElement("div");
  actions.className = "instance-actions";
  actions.append(
    button("↑", "up", "Move up"),
    button("↓", "down", "Move down"),
    button("Edit", "edit", "Edit instance"),
    button("Delete", "remove", "Delete instance"),
  );

  if (index === 0) {
    (actions.querySelector('[data-action="up"]') as HTMLButtonElement).disabled =
      true;
  }
  if (index === total - 1) {
    (actions.querySelector('[data-action="down"]') as HTMLButtonElement).disabled =
      true;
  }

  item.append(main, actions);
  return item;
}

export function renderSidebar(
  root: HTMLElement,
  scope: Role,
  instances: Instance[],
  health: Map<string, HealthReport>,
  activeId: string | null,
  actions: SidebarActions,
): void {
  root.replaceChildren();

  const switcher = document.createElement("div");
  switcher.className = "scope-switch";
  switcher.setAttribute("role", "tablist");
  switcher.setAttribute("aria-label", "View");
  for (const candidate of SCOPES) {
    const option = document.createElement("button");
    option.type = "button";
    option.className = `scope-option${candidate.value === scope ? " active" : ""}`;
    option.dataset.scope = candidate.value;
    option.setAttribute("role", "tab");
    option.setAttribute("aria-selected", String(candidate.value === scope));
    option.title = `${candidate.label} view (Cmd+Shift+${candidate.shortcut})`;
    option.textContent = candidate.label;
    switcher.append(option);
  }

  const header = document.createElement("div");
  header.className = "sidebar-header";

  const summary = document.createElement("span");
  summary.className = "scope-summary";
  summary.textContent = countLabel(scope, instances.length);

  const add = document.createElement("button");
  add.type = "button";
  add.className = "primary-button";
  add.dataset.action = "add";
  add.textContent = "Add";

  header.append(summary, add);

  const list = document.createElement("ul");
  list.className = "instance-list";

  const top = document.createElement("div");
  top.className = "sidebar-top";
  top.append(switcher);

  // The count/Add row sits below the band, so the band can be exactly as tall
  // as the tab strip and the two colour lines meet at one y.
  const body = document.createElement("div");
  body.className = "sidebar-body";
  body.append(header);

  if (instances.length === 0) {
    const empty = document.createElement("p");
    empty.className = "sidebar-empty";
    empty.textContent = emptyLabel(scope);
    body.append(empty);
  } else {
    instances.forEach((instance, index) => {
      list.append(row(instance, index, instances.length, health, activeId));
    });
    body.append(list);
  }

  root.append(top, body);

  root.onclick = (event) => {
    const element = event.target as HTMLElement;
    const scopeButton = element.closest<HTMLButtonElement>("button[data-scope]");
    if (scopeButton !== null) {
      actions.onScope(scopeButton.dataset.scope as Role);
      return;
    }
    const button = element.closest<HTMLButtonElement>("button[data-action]");
    if (button === null) {
      const owner = element.closest<HTMLLIElement>("li[data-id]");
      if (owner === null) return;
      const instance = instances.find(
        (candidate) => candidate.id === owner.dataset.id,
      );
      if (instance !== undefined) actions.onOpen(instance);
      return;
    }
    if (button.dataset.action === "add") {
      actions.onAdd();
      return;
    }
    const owner = button.closest<HTMLLIElement>("li[data-id]");
    if (owner === null) return;
    const instance = instances.find(
      (candidate) => candidate.id === owner.dataset.id,
    );
    if (instance === undefined) return;

    switch (button.dataset.action) {
      case "edit":
        actions.onEdit(instance);
        break;
      case "remove":
        actions.onRemove(instance);
        break;
      case "up":
        actions.onMove(instance, -1);
        break;
      case "down":
        actions.onMove(instance, 1);
        break;
      default:
        break;
    }
  };
}
