export type InstanceKind =
  | "dsh-web"
  | "opencode"
  | "other"
  | "youtrack"
  | "flowhub";

/// Which of the three views an instance belongs to. Derived from the kind in
/// Rust; the shell keeps the same table so a row is never listed twice.
export type Role = "upstream" | "downstream" | "hub";

export function roleOf(kind: InstanceKind): Role {
  switch (kind) {
    case "youtrack":
      return "upstream";
    case "flowhub":
      return "hub";
    default:
      return "downstream";
  }
}

export type AuthMode = "none" | "dsh-token";

export interface Instance {
  id: string;
  name: string;
  kind: InstanceKind;
  url: string;
  auth: AuthMode;
  group: string | null;
  order: number;
  notes: string;
}

export interface InstanceInput {
  name: string;
  kind: InstanceKind;
  url: string;
  group?: string | null;
  notes?: string;
}

export function kindLabel(kind: InstanceKind): string {
  switch (kind) {
    case "dsh-web":
      return "dsh web";
    case "opencode":
      return "opencode";
    case "youtrack":
      return "YouTrack";
    case "flowhub":
      return "FlowHub";
    default:
      return "other";
  }
}

export interface TabInfo {
  instance_id: string;
  instance_name: string;
  role: Role;
  title: string;
  url: string;
  loading: boolean;
  auth_required: boolean;
  loaded: boolean;
  active: boolean;
}

export interface TabStateEvent {
  instance_id: string;
  url: string;
  loading: boolean;
}

export interface TabTitleEvent {
  instance_id: string;
  title: string;
}

export interface AuthRequiredEvent {
  instance_id: string;
  instance_name: string;
}

export interface AuthOkEvent {
  instance_id: string;
}

export interface AuthFailedEvent {
  instance_id: string;
  reason: string;
}

export type HealthStatus = "ok" | "auth" | "http" | "unreachable";

/// What the probe carried: the tab's dsh cookie, or nothing.
export type HealthCredential = "none" | "cookie";

export interface HealthReport {
  instance_id: string;
  status: HealthStatus;
  latency_ms: number;
  credential: HealthCredential;
}

/// The typed scalars plus the four operational reports from `/healthz`.
export interface HubSnapshot {
  status: string;
  version: string;
  uptime_seconds: number;
  addr: string;
  data_dir: string;
  listens_on_all_interfaces: boolean;
  audit_file: string;
  payload_log: string;
  app_log: string;
  queue_depth: number;
  queue_capacity: number;
  dedupe_keys: number;
  projects: HubProjects | null;
  dispatch: HubDispatch | null;
  runtimes: HubRuntimes | null;
  counters: HubCounters | null;
  flows: HubFlowsHealth | null;
}

export interface HubFlowsHealth {
  enabled?: boolean;
  mode?: string;
  definitions?: number;
  runs?: number;
  waiting?: number;
  needs_attention?: number;
  submitted?: number;
  dropped?: number;
  failed?: number;
  flow?: string;
  projects?: string[];
  updated_at?: string;
  updated_by?: string;
  dir?: string;
  state_error?: string;
}

export interface HubProjects {
  file?: string;
  mappings?: number;
  routable?: number;
  keys?: string[];
}

export interface HubRuntimeDispatch {
  pending?: number;
  running?: number;
  breadth?: number;
  in_flight?: number;
  last_error?: string;
}

export interface HubDispatch {
  enabled?: boolean;
  queued?: number;
  handled?: number;
  ignored?: number;
  dropped?: number;
  queue_depth?: number;
  paused?: boolean;
  agent?: string;
  in_flight?: Record<string, number>;
  runtimes?: Record<string, HubRuntimeDispatch>;
}

export interface HubRuntimes {
  enabled?: boolean;
  file?: string;
  counts?: Record<string, number>;
  runtimes?: string[];
  states?: Record<string, string>;
}

export interface HubCounters {
  received?: number;
  accepted?: number;
  rejected?: number;
  duplicates?: number;
  persisted?: number;
  persist_errors?: number;
  dropped?: number;
  reject_reasons?: Record<string, number>;
}

export type HubLogState =
  | "ok"
  | "needs-path"
  | "disabled"
  | "missing"
  | "unreadable";

export interface HubLogs {
  state: HubLogState;
  path: string;
  service_path: string;
  can_use_service_path: boolean;
  lines: string[];
  truncated: boolean;
  detail: string;
}

export interface SessionView {
  sidebar_visible: boolean;
  active: string | null;
  tabs: string[];
}

/// A trigger from the flow definition, as the API summarises it.
export interface HubFlowTrigger {
  id: string;
  event: string;
  exclusive: boolean;
  priority: number;
}

/// A run summary from `/hub/v1/runs` and the flow's `last_run`.
export interface HubRunSummary {
  id: string;
  flow: string;
  state: string;
  mode: string;
  trigger: string;
  event_kind?: string;
  action: string;
  task: string;
  started_at: string;
  finished_at: string | null;
  duration_ms: number;
  error: string;
}

export interface HubFlowSummary {
  id: string;
  name: string;
  builtin: boolean;
  upstream: string;
  revision: string;
  mode: string;
  projects?: string[];
  updated_at?: string;
  updated_by?: string;
  on_available?: boolean;
  triggers: HubFlowTrigger[];
  last_run: HubRunSummary | null;
}

export interface HubFlowsResponse {
  flows: HubFlowSummary[];
}

export interface FlowNodeDefinition {
  id: string;
  type: string;
  name?: string;
  config?: Record<string, unknown>;
  next?: Array<{ to: string; when?: string }>;
}

export interface FlowEdgeDefinition {
  from: string;
  to: string;
  when?: string;
}

export interface FlowDefinition {
  version: number;
  id: string;
  name: string;
  upstream: string;
  description?: string;
  start: string;
  tasks: Record<string, unknown>;
  triggers: HubFlowTrigger[];
  nodes: FlowNodeDefinition[];
  edges: FlowEdgeDefinition[];
}

export interface HubFlowDefinitionResponse {
  id: string;
  name: string;
  builtin: boolean;
  upstream: string;
  revision: string;
  mode: string;
  projects?: string[];
  updated_at?: string;
  updated_by?: string;
  definition: FlowDefinition;
}

export interface HubRunsResponse {
  runs: HubRunSummary[];
  next_cursor: string;
}

export interface FlowStep {
  id: string;
  type: string;
  state: string;
  attempt: number;
  started_at: string;
  finished_at: string | null;
  output?: Record<string, unknown> | null;
  error?: string;
}

export interface FlowRun {
  id: string;
  flow: string;
  revision: string;
  mode: string;
  state: string;
  trigger: string;
  event: {
    id: string;
    upstream: string;
    kind: string;
    subject: { project: string; key: string; title: string; url: string };
    actor: { login: string; display?: string };
  };
  started_at: string;
  finished_at: string | null;
  steps: Record<string, FlowStep>;
  error?: string;
}

export interface HubRunResponse {
  run: FlowRun;
}
