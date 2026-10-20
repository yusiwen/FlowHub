export type InstanceKind = "dsh-web" | "opencode" | "other" | "youtrack";

/// Which of the three views an instance belongs to. Derived from the kind in
/// Rust; the shell keeps the same table so a row is never listed twice.
export type Role = "upstream" | "downstream";

export function roleOf(kind: InstanceKind): Role {
  return kind === "youtrack" ? "upstream" : "downstream";
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

export interface SessionView {
  sidebar_visible: boolean;
  active: string | null;
  tabs: string[];
}
