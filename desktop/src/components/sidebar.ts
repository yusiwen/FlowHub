import type { Instance } from "../types";
import { kindLabel } from "../types";

export interface SidebarActions {
  onAdd: () => void;
  onEdit: (instance: Instance) => void;
  onRemove: (instance: Instance) => void;
  onMove: (instance: Instance, direction: -1 | 1) => void;
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

function row(
  instance: Instance,
  index: number,
  total: number,
): HTMLLIElement {
  const item = document.createElement("li");
  item.className = "instance";
  item.dataset.id = instance.id;

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

  heading.append(name, badge);

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
  instances: Instance[],
  actions: SidebarActions,
): void {
  root.replaceChildren();

  const header = document.createElement("div");
  header.className = "sidebar-header";

  const title = document.createElement("h1");
  title.textContent = "Instances";

  const add = document.createElement("button");
  add.type = "button";
  add.className = "primary-button";
  add.dataset.action = "add";
  add.textContent = "Add";

  header.append(title, add);

  const list = document.createElement("ul");
  list.className = "instance-list";

  if (instances.length === 0) {
    const empty = document.createElement("p");
    empty.className = "sidebar-empty";
    empty.textContent =
      "No instances yet. Add the dsh web and opencode URLs you use.";
    root.append(header, empty);
    root.onclick = (event) => {
      const target = event.target as HTMLElement;
      if (target.closest('[data-action="add"]') !== null) actions.onAdd();
    };
    return;
  }

  instances.forEach((instance, index) => {
    list.append(row(instance, index, instances.length));
  });
  root.append(header, list);

  root.onclick = (event) => {
    const target = (event.target as HTMLElement).closest<HTMLButtonElement>(
      "button[data-action]",
    );
    if (target === null) return;
    if (target.dataset.action === "add") {
      actions.onAdd();
      return;
    }
    const owner = target.closest<HTMLLIElement>("li[data-id]");
    if (owner === null) return;
    const instance = instances.find(
      (candidate) => candidate.id === owner.dataset.id,
    );
    if (instance === undefined) return;

    switch (target.dataset.action) {
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
