use crate::auth;
use crate::model::{AuthMode, Instance, InstanceInput};
use crate::state::{lock_tabs, AppState};
use crate::store::lock;
use crate::tabs::{StageBounds, TabInfo};
use serde::Serialize;
use std::collections::HashSet;
use std::time::{Duration, Instant};
use tauri::{AppHandle, Emitter, Manager, State};
use url::Url;
use uuid::Uuid;

#[derive(Clone, Serialize)]
struct AuthOkEvent {
    instance_id: Uuid,
}

#[derive(Clone, Serialize)]
struct AuthFailedEvent {
    instance_id: Uuid,
    reason: String,
}

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

#[tauri::command]
pub fn submit_token(
    id: Uuid,
    input: String,
    state: State<'_, AppState>,
    app: AppHandle,
) -> Result<(), String> {
    let instance = {
        let guard = lock(&state.store)?;
        guard
            .instances()
            .iter()
            .find(|instance| instance.id == id)
            .cloned()
            .ok_or_else(|| format!("instance not found: {id}"))?
    };
    if instance.auth != AuthMode::DshToken {
        return Err("this instance does not use a dsh launch token".to_string());
    }
    let base =
        Url::parse(&instance.url).map_err(|error| format!("invalid instance url: {error}"))?;
    let token = auth::parse_token_input(&input, &base)?;
    let target = auth::token_url(&base, &token);
    lock_tabs(&state)?.begin_auth(id, target)?;

    let app_handle = app.clone();
    std::thread::spawn(move || poll_auth(app_handle, id));
    Ok(())
}

fn poll_auth(app: AppHandle, id: Uuid) {
    let deadline = Instant::now() + Duration::from_secs(15);
    loop {
        std::thread::sleep(Duration::from_millis(200));
        let state = app.state::<AppState>();
        let status = lock_tabs(&state).ok().and_then(|tabs| tabs.auth_status(id));
        if let Some((has_token, has_cookie)) = status {
            if !has_token && has_cookie {
                if let Ok(mut tabs) = lock_tabs(&state) {
                    let _ = tabs.complete_auth(id);
                }
                let _ = app.emit("auth://ok", AuthOkEvent { instance_id: id });
                return;
            }
        }
        if Instant::now() >= deadline {
            let _ = app.emit(
                "auth://failed",
                AuthFailedEvent {
                    instance_id: id,
                    reason: "the token was not accepted; paste the URL printed by dsh web again"
                        .to_string(),
                },
            );
            return;
        }
    }
}
