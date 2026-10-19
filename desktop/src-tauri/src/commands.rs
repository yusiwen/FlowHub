use crate::model::{Instance, InstanceInput};
use crate::state::{lock_tabs, AppState};
use crate::store::lock;
use crate::tabs::{StageBounds, TabInfo};
use std::collections::HashSet;
use tauri::{AppHandle, Emitter, State};
use uuid::Uuid;

fn snapshot(state: &AppState) -> Result<Vec<Instance>, String> {
    let guard = lock(&state.store)?;
    Ok(guard.instances().to_vec())
}

fn broadcast_instances(app: &AppHandle, instances: Vec<Instance>) -> Result<(), String> {
    app.emit("instances://changed", instances)
        .map_err(|error| error.to_string())
}

fn tabs_snapshot(state: &AppState) -> Result<Vec<TabInfo>, String> {
    Ok(lock_tabs(state)?.list())
}

fn broadcast_tabs(app: &AppHandle, state: &AppState) -> Result<(), String> {
    let tabs = tabs_snapshot(state)?;
    app.emit("tabs://changed", tabs)
        .map_err(|error| error.to_string())
}

pub(crate) fn open_instance(
    app: &AppHandle,
    state: &AppState,
    id: Uuid,
) -> Result<TabInfo, String> {
    let instance = {
        let guard = lock(&state.store)?;
        guard
            .instances()
            .iter()
            .find(|instance| instance.id == id)
            .cloned()
            .ok_or_else(|| format!("instance not found: {id}"))?
    };
    let info = lock_tabs(state)?.open(app, &instance)?;
    broadcast_tabs(app, state)?;
    Ok(info)
}

/// Close the active tab. `false` means there was no tab, so the caller can
/// close the window instead.
pub(crate) fn close_active(app: &AppHandle, state: &AppState) -> Result<bool, String> {
    let closed = lock_tabs(state)?.close_active(app)?;
    broadcast_tabs(app, state)?;
    Ok(closed)
}

pub(crate) fn cycle_tab(app: &AppHandle, state: &AppState, delta: isize) -> Result<(), String> {
    lock_tabs(state)?.cycle(delta)?;
    broadcast_tabs(app, state)
}

pub(crate) fn activate_index(
    app: &AppHandle,
    state: &AppState,
    index: usize,
) -> Result<(), String> {
    lock_tabs(state)?.activate_index(index)?;
    broadcast_tabs(app, state)
}

pub(crate) fn reload_active(state: &AppState) -> Result<(), String> {
    lock_tabs(state)?.reload_active()
}

pub(crate) fn open_in_browser(state: &AppState) -> Result<(), String> {
    let url = lock_tabs(state)?
        .active_url()
        .ok_or_else(|| "no active tab".to_string())?;
    std::process::Command::new("open")
        .arg(url)
        .spawn()
        .map(|_| ())
        .map_err(|error| format!("open in browser: {error}"))
}

/// Cmd+T: open the first instance that has no tab. When every instance is
/// already open, cycle to the next tab instead of creating a duplicate (one
/// instance owns one tab).
pub(crate) fn open_next_instance(
    app: &AppHandle,
    state: &AppState,
) -> Result<Option<TabInfo>, String> {
    let instances = snapshot(state)?;
    if instances.is_empty() {
        return Ok(None);
    }
    let open: HashSet<Uuid> = lock_tabs(state)?
        .list()
        .iter()
        .map(|tab| tab.instance_id)
        .collect();
    if let Some(instance) = instances
        .iter()
        .find(|instance| !open.contains(&instance.id))
    {
        return open_instance(app, state, instance.id).map(Some);
    }
    lock_tabs(state)?.cycle(1)?;
    broadcast_tabs(app, state)?;
    Ok(None)
}

#[tauri::command]
pub fn list_instances(state: State<'_, AppState>) -> Result<Vec<Instance>, String> {
    snapshot(&state)
}

#[tauri::command]
pub fn add_instance(
    input: InstanceInput,
    state: State<'_, AppState>,
    app: AppHandle,
) -> Result<Instance, String> {
    let instance = lock(&state.store)?
        .add(&input)
        .map_err(|error| error.to_string())?;
    broadcast_instances(&app, snapshot(&state)?)?;
    Ok(instance)
}

#[tauri::command]
pub fn update_instance(
    id: Uuid,
    input: InstanceInput,
    state: State<'_, AppState>,
    app: AppHandle,
) -> Result<Instance, String> {
    let instance = lock(&state.store)?
        .update(id, &input)
        .map_err(|error| error.to_string())?;
    lock_tabs(&state)?.rename(id, &instance.name);
    broadcast_instances(&app, snapshot(&state)?)?;
    broadcast_tabs(&app, &state)?;
    Ok(instance)
}

#[tauri::command]
pub fn remove_instance(id: Uuid, state: State<'_, AppState>, app: AppHandle) -> Result<(), String> {
    lock_tabs(&state)?.close_for_instance(id)?;
    lock(&state.store)?
        .remove(id)
        .map_err(|error| error.to_string())?;
    broadcast_instances(&app, snapshot(&state)?)?;
    broadcast_tabs(&app, &state)
}

#[tauri::command]
pub fn list_tabs(state: State<'_, AppState>) -> Result<Vec<TabInfo>, String> {
    tabs_snapshot(&state)
}

#[tauri::command]
pub fn open_tab(id: Uuid, state: State<'_, AppState>, app: AppHandle) -> Result<TabInfo, String> {
    open_instance(&app, &state, id)
}

#[tauri::command]
pub fn close_tab(id: Uuid, state: State<'_, AppState>, app: AppHandle) -> Result<(), String> {
    lock_tabs(&state)?.close(id)?;
    broadcast_tabs(&app, &state)
}

#[tauri::command]
pub fn activate_tab(id: Uuid, state: State<'_, AppState>, app: AppHandle) -> Result<(), String> {
    lock_tabs(&state)?.activate(id)?;
    broadcast_tabs(&app, &state)
}

#[tauri::command]
pub fn set_stage_bounds(
    x: f64,
    y: f64,
    width: f64,
    height: f64,
    state: State<'_, AppState>,
) -> Result<(), String> {
    let bounds = StageBounds::new(x, y, width, height)?;
    lock_tabs(&state)?.set_bounds(bounds)
}

#[tauri::command]
pub fn set_content_visible(
    visible: bool,
    state: State<'_, AppState>,
    app: AppHandle,
) -> Result<(), String> {
    if visible {
        lock_tabs(&state)?.show_content()
    } else {
        lock_tabs(&state)?.hide_content(&app)
    }
}

#[tauri::command]
pub fn reorder_instances(
    ids: Vec<Uuid>,
    state: State<'_, AppState>,
    app: AppHandle,
) -> Result<(), String> {
    lock(&state.store)?
        .reorder(&ids)
        .map_err(|error| error.to_string())?;
    broadcast_instances(&app, snapshot(&state)?)
}
