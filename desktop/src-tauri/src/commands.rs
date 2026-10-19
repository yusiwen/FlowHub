use crate::model::{Instance, InstanceInput};
use crate::state::{lock_tabs, AppState};
use crate::store::lock;
use crate::tabs::{StageBounds, TabInfo};
use tauri::{AppHandle, Emitter, State};
use uuid::Uuid;

fn snapshot(state: &State<'_, AppState>) -> Result<Vec<Instance>, String> {
    let guard = lock(&state.store)?;
    Ok(guard.instances().to_vec())
}

fn broadcast(app: &AppHandle, instances: Vec<Instance>) -> Result<(), String> {
    app.emit("instances://changed", instances)
        .map_err(|error| error.to_string())
}

fn tabs_snapshot(state: &State<'_, AppState>) -> Result<Vec<TabInfo>, String> {
    Ok(lock_tabs(state)?.list())
}

fn broadcast_tabs(app: &AppHandle, state: &State<'_, AppState>) -> Result<(), String> {
    let tabs = tabs_snapshot(state)?;
    app.emit("tabs://changed", tabs)
        .map_err(|error| error.to_string())
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
    broadcast(&app, snapshot(&state)?)?;
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
    broadcast(&app, snapshot(&state)?)?;
    broadcast_tabs(&app, &state)?;
    Ok(instance)
}

#[tauri::command]
pub fn remove_instance(id: Uuid, state: State<'_, AppState>, app: AppHandle) -> Result<(), String> {
    lock_tabs(&state)?.close_for_instance(id)?;
    lock(&state.store)?
        .remove(id)
        .map_err(|error| error.to_string())?;
    broadcast(&app, snapshot(&state)?)?;
    broadcast_tabs(&app, &state)
}

#[tauri::command]
pub fn list_tabs(state: State<'_, AppState>) -> Result<Vec<TabInfo>, String> {
    tabs_snapshot(&state)
}

#[tauri::command]
pub fn open_tab(id: Uuid, state: State<'_, AppState>, app: AppHandle) -> Result<TabInfo, String> {
    let instance = {
        let guard = lock(&state.store)?;
        guard
            .instances()
            .iter()
            .find(|instance| instance.id == id)
            .cloned()
            .ok_or_else(|| format!("instance not found: {id}"))?
    };
    let info = lock_tabs(&state)?.open(&app, &instance)?;
    broadcast_tabs(&app, &state)?;
    Ok(info)
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
    broadcast(&app, snapshot(&state)?)
}
