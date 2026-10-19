use crate::model::{Instance, InstanceInput};
use crate::state::AppState;
use crate::store::lock;
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
    broadcast(&app, snapshot(&state)?)?;
    Ok(instance)
}

#[tauri::command]
pub fn remove_instance(id: Uuid, state: State<'_, AppState>, app: AppHandle) -> Result<(), String> {
    lock(&state.store)?
        .remove(id)
        .map_err(|error| error.to_string())?;
    broadcast(&app, snapshot(&state)?)
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
