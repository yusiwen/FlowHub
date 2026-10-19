export type InstanceKind = "dsh-web" | "opencode" | "other";
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
    default:
      return "other";
  }
}

export interface TabInfo {
  instance_id: string;
  instance_name: string;
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

export interface HealthReport {
  instance_id: string;
  status: HealthStatus;
  latency_ms: number;
}

export interface SessionView {
  sidebar_visible: boolean;
  active: string | null;
  tabs: string[];
}
