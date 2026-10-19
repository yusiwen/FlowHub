import { invoke } from "@tauri-apps/api/core";
import { listen, type UnlistenFn } from "@tauri-apps/api/event";
import type {
  Instance,
  InstanceInput,
  TabInfo,
  TabStateEvent,
  TabTitleEvent,
  AuthRequiredEvent,
  AuthOkEvent,
  AuthFailedEvent,
} from "./types";

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

export const listTabs = (): Promise<TabInfo[]> => invoke<TabInfo[]>("list_tabs");

export const openTab = (id: string): Promise<TabInfo> =>
  invoke<TabInfo>("open_tab", { id });

export const closeTab = (id: string): Promise<void> =>
  invoke<void>("close_tab", { id });

export const activateTab = (id: string): Promise<void> =>
  invoke<void>("activate_tab", { id });

export const setStageBounds = (
  x: number,
  y: number,
  width: number,
  height: number,
): Promise<void> => invoke<void>("set_stage_bounds", { x, y, width, height });

export const setContentVisible = (visible: boolean): Promise<void> =>
  invoke<void>("set_content_visible", { visible });

export const onTabsChanged = (
  handler: (tabs: TabInfo[]) => void,
): Promise<UnlistenFn> =>
  listen<TabInfo[]>("tabs://changed", (event) => handler(event.payload));

export const onTabState = (
  handler: (state: TabStateEvent) => void,
): Promise<UnlistenFn> =>
  listen<TabStateEvent>("tab://state", (event) => handler(event.payload));

export const onTabTitle = (
  handler: (title: TabTitleEvent) => void,
): Promise<UnlistenFn> =>
  listen<TabTitleEvent>("tab://title", (event) => handler(event.payload));

export const onToggleSidebar = (handler: () => void): Promise<UnlistenFn> =>
  listen("menu://toggle-sidebar", () => handler());

export const submitToken = (id: string, input: string): Promise<void> =>
  invoke<void>("submit_token", { id, input });

export const onAuthRequired = (
  handler: (event: AuthRequiredEvent) => void,
): Promise<UnlistenFn> =>
  listen<AuthRequiredEvent>("auth://required", (event) =>
    handler(event.payload),
  );

export const onAuthOk = (
  handler: (event: AuthOkEvent) => void,
): Promise<UnlistenFn> =>
  listen<AuthOkEvent>("auth://ok", (event) => handler(event.payload));

export const onAuthFailed = (
  handler: (event: AuthFailedEvent) => void,
): Promise<UnlistenFn> =>
  listen<AuthFailedEvent>("auth://failed", (event) =>
    handler(event.payload),
  );
