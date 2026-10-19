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
