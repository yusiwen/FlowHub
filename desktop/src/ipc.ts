import { invoke } from "@tauri-apps/api/core";
import { listen, type UnlistenFn } from "@tauri-apps/api/event";
import type { Instance, InstanceInput } from "./types";

export const listInstances = (): Promise<Instance[]> =>
  invoke<Instance[]>("list_instances");

export const addInstance = (input: InstanceInput): Promise<Instance> =>
  invoke<Instance>("add_instance", { input });

export const updateInstance = (
  id: string,
  input: InstanceInput,
): Promise<Instance> => invoke<Instance>("update_instance", { id, input });

export const removeInstance = (id: string): Promise<void> =>
  invoke<void>("remove_instance", { id });

export const reorderInstances = (ids: string[]): Promise<void> =>
  invoke<void>("reorder_instances", { ids });

export const onInstancesChanged = (
  handler: (instances: Instance[]) => void,
): Promise<UnlistenFn> =>
  listen<Instance[]>("instances://changed", (event) => handler(event.payload));
