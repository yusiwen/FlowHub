import type {
  FlowDefinition,
  FlowRun,
  FlowStep,
  HubFlowSummary,
  HubRunSummary,
} from "./types";

export interface LayoutNode {
  id: string;
  type: string;
  label: string;
  x: number;
  y: number;
  width: number;
  height: number;
}

export interface LayoutEdge {
  from: string;
  to: string;
  points: Array<[number, number]>;
}

export interface FlowLayout {
  nodes: LayoutNode[];
  edges: LayoutEdge[];
  width: number;
  height: number;
}

const NODE_WIDTH = 154;
const NODE_HEIGHT = 54;
const COLUMN_GAP = 54;
const ROW_GAP = 22;

/// A DAG layout: every node sits one column right of its deepest parent, and the
/// rows are the declaration order within that column. Stable, deterministic, and
/// readable for the eleven-node builtin; the editor in M4 will replace this with
/// a real layout engine, not change the renderer.
export function layoutFlow(flow: FlowDefinition): FlowLayout {
  const order = new Map<string, number>();
  flow.nodes.forEach((node, index) => order.set(node.id, index));
  const depth = new Map<string, number>();
  for (const node of flow.nodes) depth.set(node.id, 0);
  const outgoing = new Map<string, string[]>();
  const incoming = new Map<string, string[]>();
  for (const node of flow.nodes) {
    outgoing.set(node.id, []);
    incoming.set(node.id, []);
  }
  for (const edge of flow.edges) {
    if (!outgoing.has(edge.from) || !incoming.has(edge.to)) continue;
    outgoing.get(edge.from)!.push(edge.to);
    incoming.get(edge.to)!.push(edge.from);
  }
  const queue = flow.nodes
    .filter((node) => (incoming.get(node.id) ?? []).length === 0)
    .map((node) => node.id);
  const seen = new Set<string>();
  while (queue.length > 0) {
    const id = queue.shift()!;
    if (seen.has(id)) continue;
    seen.add(id);
    for (const next of outgoing.get(id) ?? []) {
      depth.set(next, Math.max(depth.get(next) ?? 0, (depth.get(id) ?? 0) + 1));
      queue.push(next);
    }
  }
  const columns = new Map<number, string[]>();
  for (const node of flow.nodes) {
    const column = depth.get(node.id) ?? 0;
    if (!columns.has(column)) columns.set(column, []);
    columns.get(column)!.push(node.id);
  }
  for (const ids of columns.values()) {
    ids.sort((a, b) => (order.get(a) ?? 0) - (order.get(b) ?? 0));
  }
  const nodes: LayoutNode[] = [];
  const positions = new Map<string, LayoutNode>();
  for (const [column, ids] of [...columns.entries()].sort((a, b) => a[0] - b[0])) {
    ids.forEach((id, row) => {
      const node = flow.nodes.find((candidate) => candidate.id === id)!;
      const positioned: LayoutNode = {
        id,
        type: node.type,
        label: node.name?.trim() || id,
        x: 24 + column * (NODE_WIDTH + COLUMN_GAP),
        y: 24 + row * (NODE_HEIGHT + ROW_GAP),
        width: NODE_WIDTH,
        height: NODE_HEIGHT,
      };
      nodes.push(positioned);
      positions.set(id, positioned);
    });
  }
  const edges: LayoutEdge[] = [];
  for (const edge of flow.edges) {
    const from = positions.get(edge.from);
    const to = positions.get(edge.to);
    if (!from || !to) continue;
    const x1 = from.x + from.width;
    const y1 = from.y + from.height / 2;
    const x2 = to.x;
    const y2 = to.y + to.height / 2;
    const mid = x1 + Math.max(18, (x2 - x1) / 2);
    edges.push({
      from: edge.from,
      to: edge.to,
      points: [
        [x1, y1],
        [mid, y1],
        [mid, y2],
        [x2, y2],
      ],
    });
  }
  const width = Math.max(...nodes.map((node) => node.x + node.width), 300) + 24;
  const height = Math.max(...nodes.map((node) => node.y + node.height), 120) + 24;
  return { nodes, edges, width, height };
}

function svgElement<K extends keyof SVGElementTagNameMap>(
  name: K,
): SVGElementTagNameMap[K] {
  return document.createElementNS("http://www.w3.org/2000/svg", name);
}

function takenEdges(run: FlowRun | null): Set<string> {
  const taken = new Set<string>();
  if (!run) return taken;
  for (const step of Object.values(run.steps)) {
    if (step.type !== "branch") continue;
    const selected = step.output?.selected;
    if (typeof selected === "string") taken.add(`${step.id}->${selected}`);
  }
  return taken;
}

export interface GraphHandlers {
  onSelect: (id: string) => void;
}

export function renderGraph(
  svg: SVGSVGElement,
  layout: FlowLayout,
  run: FlowRun | null,
  selectedId: string | null,
  handlers: GraphHandlers,
): void {
  svg.replaceChildren();
  svg.setAttribute("viewBox", `0 0 ${layout.width} ${layout.height}`);
  svg.setAttribute("role", "img");
  svg.setAttribute("aria-label", "Flow graph");
  const defs = svgElement("defs");
  const marker = svgElement("marker");
  marker.setAttribute("id", "flow-arrow");
  marker.setAttribute("viewBox", "0 0 10 10");
  marker.setAttribute("refX", "9");
  marker.setAttribute("refY", "5");
  marker.setAttribute("markerWidth", "6");
  marker.setAttribute("markerHeight", "6");
  marker.setAttribute("orient", "auto-start-reverse");
  const arrow = svgElement("path");
  arrow.setAttribute("d", "M 0 0 L 10 5 L 0 10 z");
  arrow.setAttribute("class", "flow-arrow");
  marker.append(arrow);
  defs.append(marker);
  svg.append(defs);

  const taken = takenEdges(run);
  const branches = new Set(
    run
      ? Object.values(run.steps)
          .filter((step) => step.type === "branch" && typeof step.output?.selected === "string")
          .map((step) => step.id)
      : [],
  );

  for (const edge of layout.edges) {
    const path = svgElement("path");
    path.setAttribute(
      "d",
      edge.points.map(([x, y], index) => `${index === 0 ? "M" : "L"} ${x} ${y}`).join(" "),
    );
    path.setAttribute("class", "flow-edge");
    path.setAttribute("marker-end", "url(#flow-arrow)");
    if (run) {
      if (taken.has(`${edge.from}->${edge.to}`)) {
        path.classList.add("taken");
      } else if (branches.has(edge.from)) {
        path.classList.add("dim");
      }
    }
    svg.append(path);
  }

  for (const node of layout.nodes) {
    const group = svgElement("g");
    group.setAttribute("class", "flow-node");
    group.setAttribute("data-node", node.id);
    group.setAttribute("tabindex", "0");
    group.setAttribute("role", "button");
    group.setAttribute("aria-label", `${node.id} ${node.type}`);
    if (node.id === selectedId) group.classList.add("selected");
    if (run) {
      const step = run.steps[node.id];
      if (!step) group.classList.add("unvisited");
      else group.classList.add(`step-${step.state}`);
    }
    const rect = svgElement("rect");
    rect.setAttribute("x", String(node.x));
    rect.setAttribute("y", String(node.y));
    rect.setAttribute("width", String(node.width));
    rect.setAttribute("height", String(node.height));
    rect.setAttribute("rx", "8");
    group.append(rect);
    const label = svgElement("text");
    label.setAttribute("x", String(node.x + 12));
    label.setAttribute("y", String(node.y + 22));
    label.setAttribute("class", "flow-node-label");
    label.textContent = node.label;
    group.append(label);
    const type = svgElement("text");
    type.setAttribute("x", String(node.x + 12));
    type.setAttribute("y", String(node.y + 41));
    type.setAttribute("class", "flow-node-type");
    type.textContent = node.type;
    group.append(type);
    group.addEventListener("click", () => handlers.onSelect(node.id));
    group.addEventListener("keydown", (event) => {
      if (event.key === "Enter" || event.key === " ") {
        event.preventDefault();
        handlers.onSelect(node.id);
      }
    });
    svg.append(group);
  }
}

function row(container: HTMLElement, key: string, value: string, mono = false): void {
  const line = document.createElement("div");
  line.className = "flow-inspector-row";
  const label = document.createElement("span");
  label.className = "flow-inspector-key";
  label.textContent = key;
  const text = document.createElement("span");
  text.className = mono ? "flow-inspector-value mono" : "flow-inspector-value";
  text.textContent = value;
  line.append(label, text);
  container.append(line);
}

export function renderInspector(
  container: HTMLElement,
  flow: HubFlowSummary,
  definition: FlowDefinition | null,
  run: FlowRun | null,
  selectedId: string | null,
): void {
  container.replaceChildren();
  const heading = document.createElement("h3");
  if (!selectedId) {
    heading.textContent = "Flow overview";
    container.append(heading);
    row(container, "name", flow.name);
    row(container, "id", flow.id, true);
    row(container, "mode", flow.mode);
    row(container, "revision", flow.revision.slice(0, 12), true);
    row(container, "upstream", flow.upstream);
    row(container, "nodes", definition ? String(definition.nodes.length) : "—");
    row(container, "edges", definition ? String(definition.edges.length) : "—");
    const triggers = document.createElement("div");
    triggers.className = "flow-inspector-row";
    const triggerKey = document.createElement("span");
    triggerKey.className = "flow-inspector-key";
    triggerKey.textContent = "triggers";
    const triggerValue = document.createElement("span");
    triggerValue.className = "flow-inspector-value";
    triggerValue.textContent = flow.triggers
      .map(
        (trigger) =>
          `${trigger.event === "*" ? "all events" : trigger.event}${trigger.exclusive ? " · exclusive" : ""}`,
      )
      .join(", ");
    triggers.append(triggerKey, triggerValue);
    container.append(triggers);
    return;
  }
  const node = definition?.nodes.find((candidate) => candidate.id === selectedId);
  heading.textContent = selectedId;
  container.append(heading);
  row(container, "type", node?.type ?? run?.steps[selectedId]?.type ?? "unknown");
  if (node?.config) {
    const pre = document.createElement("pre");
    pre.className = "flow-inspector-config";
    pre.textContent = JSON.stringify(node.config, null, 2);
    container.append(pre);
  }
  const step = run?.steps[selectedId];
  if (!step) {
    const empty = document.createElement("p");
    empty.className = "flow-muted";
    empty.textContent = run ? "Not reached in this run." : "Select a run to overlay its status.";
    container.append(empty);
    return;
  }
  row(container, "state", step.state);
  row(container, "duration", formatDuration(durationOf(step)));
  if (step.output && Object.keys(step.output).length > 0) {
    const pre = document.createElement("pre");
    pre.className = "flow-inspector-config";
    pre.textContent = JSON.stringify(step.output, null, 2);
    container.append(pre);
  }
  if (step.error) {
    const error = document.createElement("p");
    error.className = "flow-error";
    error.textContent = step.error;
    container.append(error);
  }
}

export function renderRunList(
  container: HTMLElement,
  runs: HubRunSummary[],
  selectedId: string | null,
  onSelect: (id: string) => void,
): void {
  container.replaceChildren();
  if (runs.length === 0) {
    const empty = document.createElement("p");
    empty.className = "flow-muted";
    empty.textContent = "No runs yet.";
    container.append(empty);
    return;
  }
  for (const run of runs) {
    const button = document.createElement("button");
    button.type = "button";
    button.className = "flow-run";
    if (run.id === selectedId) button.classList.add("selected");
    button.dataset.run = run.id;
    const state = document.createElement("span");
    state.className = `flow-state state-${run.state}`;
    state.textContent = run.state;
    const time = document.createElement("span");
    time.className = "flow-run-time";
    time.textContent = relativeTime(run.started_at);
    const kind = document.createElement("span");
    kind.className = "flow-run-kind";
    kind.textContent = run.event_kind || run.trigger;
    const action = document.createElement("span");
    action.className = "flow-run-action";
    action.textContent = run.action || "—";
    const task = document.createElement("span");
    task.className = "flow-run-task mono";
    task.textContent = run.task || "—";
    const duration = document.createElement("span");
    duration.className = "flow-run-duration";
    duration.textContent = formatDuration(run.duration_ms);
    button.append(state, time, kind, action, task, duration);
    button.addEventListener("click", () => onSelect(run.id));
    container.append(button);
  }
}

export function renderStepChain(
  container: HTMLElement,
  run: FlowRun | null,
  selectedId: string | null,
  onSelect: (id: string) => void,
): void {
  container.replaceChildren();
  if (!run) {
    const empty = document.createElement("p");
    empty.className = "flow-muted";
    empty.textContent = "Select a run to see its steps.";
    container.append(empty);
    return;
  }
  const steps = Object.values(run.steps).sort(
    (a, b) => Date.parse(a.started_at) - Date.parse(b.started_at),
  );
  for (const step of steps) {
    const button = document.createElement("button");
    button.type = "button";
    button.className = "flow-step";
    if (step.id === selectedId) button.classList.add("selected");
    const state = document.createElement("span");
    state.className = `flow-state state-${step.state}`;
    state.textContent = step.state;
    const id = document.createElement("span");
    id.className = "flow-step-id";
    id.textContent = step.id;
    const type = document.createElement("span");
    type.className = "flow-step-type";
    type.textContent = step.type;
    const duration = document.createElement("span");
    duration.className = "flow-step-duration";
    duration.textContent = formatDuration(durationOf(step));
    button.append(state, id, type, duration);
    button.addEventListener("click", () => onSelect(step.id));
    container.append(button);
  }
  if (run.error) {
    const error = document.createElement("p");
    error.className = "flow-error";
    error.textContent = run.error;
    container.append(error);
  }
}

function durationOf(step: FlowStep): number {
  if (!step.finished_at) return 0;
  return Math.max(0, Date.parse(step.finished_at) - Date.parse(step.started_at));
}

export function formatDuration(ms: number): string {
  if (!Number.isFinite(ms) || ms <= 0) return "—";
  if (ms < 1000) return `${Math.round(ms)} ms`;
  if (ms < 60_000) return `${(ms / 1000).toFixed(1)} s`;
  const minutes = Math.floor(ms / 60_000);
  const seconds = Math.round((ms % 60_000) / 1000);
  return `${minutes}m ${seconds}s`;
}

export function relativeTime(iso: string | null): string {
  if (!iso) return "—";
  const then = Date.parse(iso);
  if (!Number.isFinite(then)) return "—";
  const seconds = Math.max(0, Math.round((Date.now() - then) / 1000));
  if (seconds < 5) return "just now";
  if (seconds < 60) return `${seconds}s ago`;
  if (seconds < 3600) return `${Math.floor(seconds / 60)}m ago`;
  if (seconds < 86_400) return `${Math.floor(seconds / 3600)}h ago`;
  return `${Math.floor(seconds / 86_400)}d ago`;
}

export function actionOf(run: FlowRun): string {
  return typeof run.steps.evaluate?.output?.action === "string"
    ? (run.steps.evaluate.output.action as string)
    : "";
}
