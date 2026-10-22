use crate::auth;
use crate::health::{self, HealthReport};
use crate::hub::{self, HubLogs, HubSnapshot};
use crate::hub_api;
use crate::model::{AuthMode, Instance, InstanceInput};
use crate::session::SessionView;
use crate::state::{lock_prefs, lock_session, lock_tabs, AppState};
use crate::store::lock;
use crate::tabs::{viewport_top_inset, StageBounds, TabInfo};
use crate::token_store;
use serde::Serialize;
use serde_json::Value;
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

/// Persist the current tab order and active tab. The sidebar flag comes from the
/// session store because the shell owns that toggle.
fn persist_session(state: &AppState) -> Result<(), String> {
    let (tabs, active) = lock_tabs(state)?.session_snapshot();
    let mut session = lock_session(state)?;
    let sidebar = session.sidebar_visible();
    session
        .save(sidebar, tabs, active)
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
    persist_session(state)?;
    Ok(info)
}

/// Close the active tab. `false` means there was no tab, so the caller can
/// close the window instead.
pub(crate) fn close_active(app: &AppHandle, state: &AppState) -> Result<bool, String> {
    let closed = lock_tabs(state)?.close_active(app)?;
    broadcast_tabs(app, state)?;
    persist_session(state)?;
    Ok(closed)
}

pub(crate) fn cycle_tab(app: &AppHandle, state: &AppState, delta: isize) -> Result<(), String> {
    lock_tabs(state)?.cycle(app, delta)?;
    broadcast_tabs(app, state)?;
    persist_session(state)
}

pub(crate) fn activate_index(
    app: &AppHandle,
    state: &AppState,
    index: usize,
) -> Result<(), String> {
    lock_tabs(state)?.activate_index(app, index)?;
    broadcast_tabs(app, state)?;
    persist_session(state)
}

pub(crate) fn reload_active(app: &AppHandle, state: &AppState) -> Result<(), String> {
    if lock_tabs(state)?.active_is_dashboard() {
        // The dashboard has no page to reload; ask the shell to fetch /healthz
        // again so Cmd+R means "refresh what is on screen" for both tab kinds.
        return app
            .emit("hub://reload", ())
            .map_err(|error| error.to_string());
    }
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
    if let Ok(mut prefs) = lock_prefs(&state) {
        let _ = prefs.remove(id);
    }
    token_store::remove(id);
    if let Ok(mut tokens) = state.hub_tokens.lock() {
        tokens.remove(&id);
    }
    if let Ok(mut health) = state.health.lock() {
        health.remove(&id);
    }
    broadcast_instances(&app, snapshot(&state)?)?;
    broadcast_tabs(&app, &state)?;
    persist_session(&state)
}

#[tauri::command]
pub fn list_health(state: State<'_, AppState>) -> Result<Vec<HealthReport>, String> {
    let guard = state
        .health
        .lock()
        .map_err(|_| "health lock poisoned".to_string())?;
    Ok(guard.values().cloned().collect())
}

#[tauri::command]
pub async fn probe_all(app: AppHandle) -> Result<Vec<HealthReport>, String> {
    Ok(health::probe_all(&app).await)
}

/// The Hub dashboard's one data source: the service's own /healthz. The shell
/// cannot fetch it (its CSP allows only Tauri IPC), and parsing here lets a
/// wrong address be reported as "not a FlowHub service".
#[tauri::command]
pub async fn hub_snapshot(id: Uuid, state: State<'_, AppState>) -> Result<HubSnapshot, String> {
    let instance = {
        let guard = lock(&state.store)?;
        guard
            .instances()
            .iter()
            .find(|instance| instance.id == id)
            .cloned()
            .ok_or_else(|| format!("no such instance: {id}"))?
    };
    if !instance.is_dashboard() {
        return Err("instance is not a FlowHub service".to_string());
    }
    hub::fetch(&instance.url).await
}

fn dashboard_instance(state: &AppState, id: Uuid) -> Result<Instance, String> {
    let guard = lock(&state.store)?;
    let instance = guard
        .instances()
        .iter()
        .find(|instance| instance.id == id)
        .cloned()
        .ok_or_else(|| format!("no such instance: {id}"))?;
    if !instance.is_dashboard() {
        return Err("instance is not a FlowHub service".to_string());
    }
    Ok(instance)
}

fn hub_token(
    state: &AppState,
    id: Uuid,
    instance: &Instance,
    token: Option<&str>,
    data_dir: Option<&str>,
) -> Result<String, String> {
    if let Some(token) = token.map(str::trim).filter(|token| !token.is_empty()) {
        return Ok(token.to_string());
    }
    if let Ok(tokens) = state.hub_tokens.lock() {
        if let Some(token) = tokens.get(&id) {
            return Ok(token.clone());
        }
    }
    if let Some(token) = token_store::load(id) {
        if let Ok(mut tokens) = state.hub_tokens.lock() {
            tokens.insert(id, token.clone());
        }
        return Ok(token);
    }
    hub_api::resolve_token(&instance.url, None, data_dir)
}

/// The flow overview: mode, revision, triggers and the latest run.
#[tauri::command]
pub async fn hub_flows(
    id: Uuid,
    token: Option<String>,
    data_dir: Option<String>,
    state: State<'_, AppState>,
) -> Result<Value, String> {
    let instance = dashboard_instance(&state, id)?;
    let token = hub_token(&state, id, &instance, token.as_deref(), data_dir.as_deref())?;
    hub_api::get(&instance.url, "flows", &token).await
}

/// One flow definition, used by the graph renderer.
#[tauri::command]
pub async fn hub_flow(
    id: Uuid,
    flow_id: String,
    token: Option<String>,
    data_dir: Option<String>,
    state: State<'_, AppState>,
) -> Result<Value, String> {
    let instance = dashboard_instance(&state, id)?;
    let flow_id = hub_api::segment(&flow_id)?;
    let token = hub_token(&state, id, &instance, token.as_deref(), data_dir.as_deref())?;
    hub_api::get(&instance.url, &format!("flows/{flow_id}"), &token).await
}

/// Run summaries, optionally filtered by state.
#[tauri::command]
pub async fn hub_runs(
    id: Uuid,
    token: Option<String>,
    data_dir: Option<String>,
    state_filter: Option<String>,
    limit: Option<u32>,
    state: State<'_, AppState>,
) -> Result<Value, String> {
    let instance = dashboard_instance(&state, id)?;
    let token = hub_token(&state, id, &instance, token.as_deref(), data_dir.as_deref())?;
    let mut pairs = Vec::new();
    if let Some(value) = state_filter
        .as_deref()
        .map(str::trim)
        .filter(|v| !v.is_empty())
    {
        let encoded: String = url::form_urlencoded::byte_serialize(value.as_bytes()).collect();
        pairs.push(format!("state={encoded}"));
    }
    if let Some(limit) = limit {
        pairs.push(format!("limit={}", limit.min(200)));
    }
    let path = if pairs.is_empty() {
        "runs".to_string()
    } else {
        format!("runs?{}", pairs.join("&"))
    };
    hub_api::get(&instance.url, &path, &token).await
}

/// One run with its steps, for the graph overlay and the inspector.
#[tauri::command]
pub async fn hub_run(
    id: Uuid,
    run_id: String,
    token: Option<String>,
    data_dir: Option<String>,
    state: State<'_, AppState>,
) -> Result<Value, String> {
    let instance = dashboard_instance(&state, id)?;
    let run_id = hub_api::segment(&run_id)?;
    let token = hub_token(&state, id, &instance, token.as_deref(), data_dir.as_deref())?;
    hub_api::get(&instance.url, &format!("runs/{run_id}"), &token).await
}

/// Store a pasted token in the login keychain. The value never enters the
/// registry, the shell's localStorage or the logs.
#[tauri::command]
pub fn set_hub_token(id: Uuid, token: String, state: State<'_, AppState>) -> Result<(), String> {
    dashboard_instance(&state, id)?;
    let token = token.trim();
    if token.is_empty() {
        return Err("the hub token is empty".to_string());
    }
    token_store::save(id, token)?;
    if let Ok(mut tokens) = state.hub_tokens.lock() {
        tokens.insert(id, token.to_string());
    }
    Ok(())
}

/// The Hub's log tail. The path decision lives here, not in the shell: a
/// loopback service's `app_log` is read, a remote one may only use a path the
/// user confirmed in `prefs.json`.
#[tauri::command]
pub async fn hub_logs(id: Uuid, state: State<'_, AppState>) -> Result<HubLogs, String> {
    let instance = {
        let guard = lock(&state.store)?;
        guard
            .instances()
            .iter()
            .find(|instance| instance.id == id)
            .cloned()
            .ok_or_else(|| format!("no such instance: {id}"))?
    };
    if !instance.is_dashboard() {
        return Err("instance is not a FlowHub service".to_string());
    }
    let confirmed = {
        let prefs = lock_prefs(&state)?;
        prefs.hub_log_path(id).map(ToOwned::to_owned)
    };
    hub::logs(&instance.url, confirmed.as_deref(), hub::DEFAULT_LOG_LINES).await
}

#[tauri::command]
pub fn set_hub_log_path(
    id: Uuid,
    path: Option<String>,
    state: State<'_, AppState>,
) -> Result<(), String> {
    let is_hub = {
        let guard = lock(&state.store)?;
        guard
            .instances()
            .iter()
            .find(|instance| instance.id == id)
            .map(|instance| instance.is_dashboard())
            .unwrap_or(false)
    };
    if !is_hub {
        return Err("instance is not a FlowHub service".to_string());
    }
    let trimmed = path
        .as_deref()
        .map(str::trim)
        .filter(|path| !path.is_empty());
    if let Some(path) = trimmed {
        if !std::path::Path::new(path).is_absolute() {
            return Err("the log path must be absolute".to_string());
        }
    }
    lock_prefs(&state)?.set_hub_log_path(id, trimmed)
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
    broadcast_tabs(&app, &state)?;
    persist_session(&state)
}

#[tauri::command]
pub fn activate_tab(id: Uuid, state: State<'_, AppState>, app: AppHandle) -> Result<(), String> {
    lock_tabs(&state)?.activate(&app, id)?;
    broadcast_tabs(&app, &state)?;
    persist_session(&state)
}

#[tauri::command]
pub fn set_stage_bounds(
    x: f64,
    y: f64,
    width: f64,
    height: f64,
    viewport_height: f64,
    state: State<'_, AppState>,
    app: AppHandle,
) -> Result<(), String> {
    if !viewport_height.is_finite() || viewport_height <= 0.0 {
        return Err("viewport height must be a positive finite number".to_string());
    }
    let window = app
        .get_window("main")
        .ok_or_else(|| "main window is missing".to_string())?;
    let shell = app
        .get_webview("shell")
        .ok_or_else(|| "shell webview is missing".to_string())?;
    let scale = window
        .scale_factor()
        .map_err(|error| format!("window scale factor: {error}"))?;
    let content_height = shell
        .size()
        .map_err(|error| format!("shell webview size: {error}"))?
        .to_logical::<f64>(scale)
        .height;
    if !content_height.is_finite() || content_height <= 0.0 {
        return Err("shell webview height is not measurable".to_string());
    }
    let bounds = StageBounds::new(
        x,
        y + viewport_top_inset(content_height, viewport_height),
        width,
        height,
    )?;
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
pub fn load_session(state: State<'_, AppState>) -> Result<SessionView, String> {
    Ok(lock_session(&state)?.view())
}

#[tauri::command]
pub fn set_sidebar_visible(visible: bool, state: State<'_, AppState>) -> Result<(), String> {
    let (tabs, active) = lock_tabs(&state)?.session_snapshot();
    let mut session = lock_session(&state)?;
    session
        .save(visible, tabs, active)
        .map_err(|error| error.to_string())
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
