import "./styles/base.css";
import { confirmRemove, openInstanceForm } from "./components/instance-form";
import { renderSidebar } from "./components/sidebar";
import {
  addInstance,
  listInstances,
  onInstancesChanged,
  removeInstance,
  reorderInstances,
  updateInstance,
} from "./ipc";
import type { Instance } from "./types";

const app = document.querySelector<HTMLDivElement>("#app");
if (app === null) throw new Error("shell: missing #app");

app.innerHTML = `
  <div class="layout">
    <aside class="sidebar" id="sidebar"></aside>
    <main class="stage" id="stage">
      <section class="stage-empty">
        <h2>Agent Hub</h2>
        <p>Pick an instance on the left. One native tab per instance arrives in the next slice.</p>
      </section>
    </main>
  </div>
  <p class="error" id="error" role="alert" hidden></p>
`;

const sidebar = document.querySelector<HTMLElement>("#sidebar")!;
const errorBar = document.querySelector<HTMLElement>("#error")!;

let instances: Instance[] = [];

function showError(error: unknown): void {
  errorBar.textContent = error instanceof Error ? error.message : String(error);
  errorBar.hidden = false;
}

function clearError(): void {
  errorBar.hidden = true;
}

function render(): void {
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
  });
}

async function refresh(): Promise<void> {
  try {
    instances = await listInstances();
    clearError();
    render();
  } catch (error) {
    showError(error);
  }
}

async function add(): Promise<void> {
  const input = await openInstanceForm();
  if (input === null) return;
  try {
    await addInstance(input);
    await refresh();
  } catch (error) {
    showError(error);
  }
}

async function edit(instance: Instance): Promise<void> {
  const input = await openInstanceForm(instance);
  if (input === null) return;
  try {
    await updateInstance(instance.id, input);
    await refresh();
  } catch (error) {
    showError(error);
  }
}

async function remove(instance: Instance): Promise<void> {
  if (!(await confirmRemove(instance.name))) return;
  try {
    await removeInstance(instance.id);
    await refresh();
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
    await refresh();
  } catch (error) {
    showError(error);
  }
}

void onInstancesChanged((next) => {
  instances = next;
  clearError();
  render();
});

void refresh();
