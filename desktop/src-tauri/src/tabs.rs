//! One native child WebView per instance tab.
//!
//! Content webviews are attached to the same plain window as the shell but are
//! not named in any capability, so a remote page has no Tauri IPC. The shell
//! owns the sidebar and tab bar; Rust owns the stage rectangle and applies it to
//! every child webview.
//!
//! A restored tab starts as a placeholder with no webview. Activating it creates
//! the webview; the rest stay placeholders until first use.

use crate::auth;
use crate::health;
use crate::model::{AuthMode, Instance, Role};
use serde::Serialize;
use std::collections::HashMap;
use tauri::webview::{NewWindowResponse, PageLoadEvent, Webview, WebviewBuilder};
use tauri::{AppHandle, Emitter, LogicalPosition, LogicalSize, Manager, WebviewUrl};
use url::Url;
use uuid::Uuid;

#[derive(Debug, Clone, Copy, PartialEq)]
pub struct StageBounds {
    pub x: f64,
    pub y: f64,
    pub width: f64,
    pub height: f64,
}

/// Distance from the window content view's top to the top of the shell's CSS
/// viewport.
///
/// WebKit insets a full-size window's layout viewport below the title bar, so the
/// shell measures its stage 32 points lower than the content view's origin. A
/// child webview is positioned from that origin; without this offset its page
/// covers the bottom of the tab bar. A host without the inset returns zero.
pub fn viewport_top_inset(content_height: f64, viewport_height: f64) -> f64 {
    (content_height - viewport_height).max(0.0)
}

impl StageBounds {
    pub fn new(x: f64, y: f64, width: f64, height: f64) -> Result<Self, String> {
        if !x.is_finite() || !y.is_finite() || !width.is_finite() || !height.is_finite() {
            return Err("stage bounds must be finite".to_string());
        }
        if x < 0.0 || y < 0.0 {
            return Err("stage bounds must not be negative".to_string());
        }
        if width <= 0.0 || height <= 0.0 {
            return Err("stage bounds must be positive".to_string());
        }
        Ok(Self {
            x,
            y,
            width,
            height,
        })
    }
}

#[derive(Debug, Clone, Serialize)]
pub struct TabInfo {
    pub instance_id: Uuid,
    pub instance_name: String,
    pub role: Role,
    pub title: String,
    pub url: String,
    pub loading: bool,
    pub auth_required: bool,
    pub loaded: bool,
    pub active: bool,
}

#[derive(Debug, Clone, Serialize)]
struct TabStateEvent {
    instance_id: Uuid,
    url: String,
    loading: bool,
}

#[derive(Debug, Clone, Serialize)]
struct TabTitleEvent {
    instance_id: Uuid,
    title: String,
}

#[derive(Debug, Clone, Serialize)]
struct AuthRequiredEvent {
    instance_id: Uuid,
    instance_name: String,
}

/// Pure ordering and active-tab selection, separate from the webviews so the
/// neighbour rules can be unit tested without a Tauri runtime.
#[derive(Debug, Default, PartialEq, Eq)]
pub struct TabOrder {
    order: Vec<Uuid>,
    active: Option<Uuid>,
}

impl TabOrder {
    pub fn open(&mut self, id: Uuid) -> bool {
        if self.order.contains(&id) {
            self.active = Some(id);
            return false;
        }
        self.order.push(id);
        self.active = Some(id);
        true
    }

    /// Remove one tab and return the tab that should be active afterwards.
    /// Closing a background tab leaves the active tab alone; closing the active
    /// tab selects its right neighbour, or the new last tab when it was last.
    pub fn close(&mut self, id: Uuid) -> Option<Uuid> {
        let Some(position) = self.order.iter().position(|candidate| *candidate == id) else {
            return self.active;
        };
        self.order.remove(position);
        if self.active == Some(id) {
            self.active = if self.order.is_empty() {
                None
            } else {
                Some(self.order[position.min(self.order.len() - 1)])
            };
        }
        self.active
    }

    pub fn activate(&mut self, id: Uuid) -> bool {
        if self.order.contains(&id) {
            self.active = Some(id);
            true
        } else {
            false
        }
    }

    pub fn active(&self) -> Option<Uuid> {
        self.active
    }

    pub fn ids(&self) -> &[Uuid] {
        &self.order
    }

    pub fn activate_index(&mut self, index: usize) -> Option<Uuid> {
        let id = *self.order.get(index)?;
        self.active = Some(id);
        Some(id)
    }

    pub fn cycle(&mut self, delta: isize) -> Option<Uuid> {
        if self.order.is_empty() {
            return None;
        }
        let current = self
            .active
            .and_then(|id| self.order.iter().position(|candidate| *candidate == id))
            .unwrap_or(0);
        let len = self.order.len() as isize;
        let next = (current as isize + delta).rem_euclid(len) as usize;
        self.active = Some(self.order[next]);
        self.active
    }

    pub fn clear(&mut self) {
        self.order.clear();
        self.active = None;
    }
}

fn tab_label(id: Uuid) -> String {
    format!("tab-{id}")
}

struct Tab {
    instance: Instance,
    title: String,
    url: String,
    loading: bool,
    auth_required: bool,
    webview: Option<Webview>,
}

#[derive(Default)]
pub struct TabManager {
    tabs: HashMap<Uuid, Tab>,
    order: TabOrder,
    bounds: Option<StageBounds>,
}

impl TabManager {
    /// Build placeholders from a session. No webview is created here; the active
    /// tab is loaded by `activate` when the shell asks for it.
    pub fn restore(instances: &[Instance], ids: &[Uuid], active: Option<Uuid>) -> Self {
        let mut by_id: HashMap<Uuid, &Instance> = instances
            .iter()
            .map(|instance| (instance.id, instance))
            .collect();
        let mut tabs = HashMap::new();
        let mut order = TabOrder::default();
        for id in ids {
            let Some(instance) = by_id.remove(id) else {
                continue;
            };
            tabs.insert(
                *id,
                Tab {
                    instance: instance.clone(),
                    // No page title until the webview reports one; the shell
                    // labels the tab with the instance name alone meanwhile.
                    title: String::new(),
                    url: instance.url.clone(),
                    loading: false,
                    auth_required: false,
                    webview: None,
                },
            );
            order.open(*id);
        }
        if let Some(active) = active {
            order.activate(active);
        }
        Self {
            tabs,
            order,
            bounds: None,
        }
    }

    pub fn session_snapshot(&self) -> (Vec<Uuid>, Option<Uuid>) {
        (self.order.ids().to_vec(), self.order.active())
    }

    pub fn set_bounds(&mut self, bounds: StageBounds) -> Result<(), String> {
        self.bounds = Some(bounds);
        self.apply_bounds()?;
        self.apply_visibility()
    }

    pub fn open(&mut self, app: &AppHandle, instance: &Instance) -> Result<TabInfo, String> {
        if self.tabs.contains_key(&instance.id) {
            self.activate(app, instance.id)?;
            return self.info(instance.id).ok_or("tab disappeared".to_string());
        }

        self.tabs.insert(
            instance.id,
            Tab {
                instance: instance.clone(),
                // No page title until the webview reports one; the shell labels
                // the tab with the instance name alone meanwhile.
                title: String::new(),
                url: instance.url.clone(),
                loading: false,
                auth_required: false,
                webview: None,
            },
        );
        self.order.open(instance.id);
        if let Err(error) = self.ensure_webview(app, instance.id) {
            self.tabs.remove(&instance.id);
            self.order.close(instance.id);
            return Err(error);
        }
        self.apply_visibility()?;
        self.focus_shell_if_dashboard(app)?;
        self.info(instance.id).ok_or("tab disappeared".to_string())
    }

    fn ensure_webview(&mut self, app: &AppHandle, id: Uuid) -> Result<(), String> {
        let Some(tab) = self.tabs.get(&id) else {
            return Err("no such tab".to_string());
        };
        if tab.instance.is_dashboard() {
            return Ok(());
        }
        if tab.webview.is_some() {
            return Ok(());
        }

        let bounds = self
            .bounds
            .ok_or_else(|| "stage bounds are not set yet".to_string())?;
        let window = app
            .get_window("main")
            .ok_or_else(|| "main window is missing".to_string())?;
        let url = Url::parse(&tab.instance.url)
            .map_err(|error| format!("invalid instance url: {error}"))?;
        let cookie_url = url.clone();
        let instance_id = id;
        let instance_name = tab.instance.name.clone();
        let auth_mode = tab.instance.auth;
        let label = tab_label(id);

        let state_app = app.clone();
        let title_app = app.clone();

        let builder = WebviewBuilder::new(label, WebviewUrl::External(url))
            .devtools(cfg!(debug_assertions))
            .on_navigation(|url| matches!(url.scheme(), "http" | "https"))
            .on_new_window(|_, _| NewWindowResponse::Deny)
            .on_page_load(move |_webview, payload| {
                let event = TabStateEvent {
                    instance_id,
                    url: payload.url().to_string(),
                    loading: payload.event() == PageLoadEvent::Started,
                };
                let _ = state_app.emit("tab://state", event);
            })
            .on_document_title_changed(move |_webview, title| {
                let _ = title_app.emit("tab://title", TabTitleEvent { instance_id, title });
            });

        let webview = window
            .add_child(
                builder,
                LogicalPosition::new(bounds.x, bounds.y),
                LogicalSize::new(bounds.width, bounds.height),
            )
            .map_err(|error| format!("create tab webview: {error}"))?;

        let expected_cookie = auth::cookie_name(&cookie_url);
        let auth_required = auth_mode == AuthMode::DshToken
            && !webview
                .cookies()
                .map(|cookies| {
                    cookies
                        .iter()
                        .any(|cookie| cookie.name() == expected_cookie.as_str())
                })
                .unwrap_or(false);

        {
            let tab = self.tabs.get_mut(&id).ok_or("tab disappeared")?;
            tab.webview = Some(webview);
            tab.auth_required = auth_required;
        }

        // A cookie can be present and still be dead — expired, or signed with a
        // secret the dsh data directory no longer has. Verify this tab with its
        // own cookie once, so a page that only answers 401 is replaced by the
        // token panel instead of waiting for the next periodic probe.
        if !auth_required {
            if let Some(instance) = self.tabs.get(&id).map(|tab| tab.instance.clone()) {
                health::verify_tab(app.clone(), instance);
            }
        }

        if auth_required {
            if let Some(webview) = self.tabs.get(&id).and_then(|tab| tab.webview.as_ref()) {
                webview
                    .hide()
                    .map_err(|error| format!("hide tab during sign-in: {error}"))?;
            }
            let _ = app.emit(
                "auth://required",
                AuthRequiredEvent {
                    instance_id,
                    instance_name,
                },
            );
        }
        Ok(())
    }

    pub fn close(&mut self, id: Uuid) -> Result<(), String> {
        if let Some(tab) = self.tabs.remove(&id) {
            if let Some(webview) = tab.webview {
                webview
                    .close()
                    .map_err(|error| format!("close tab webview: {error}"))?;
            }
        }
        self.order.close(id);
        self.apply_visibility()
    }

    pub fn close_for_instance(&mut self, id: Uuid) -> Result<(), String> {
        if self.tabs.contains_key(&id) {
            self.close(id)?;
        }
        Ok(())
    }

    /// Whether the active tab renders shell HTML rather than a webview. The
    /// dashboard has no webview to focus, so the shell must take focus itself.
    pub fn active_is_dashboard(&self) -> bool {
        self.order
            .active()
            .and_then(|id| self.tabs.get(&id))
            .map(|tab| tab.instance.is_dashboard())
            .unwrap_or(false)
    }

    fn focus_shell_if_dashboard(&self, app: &AppHandle) -> Result<(), String> {
        if !self.active_is_dashboard() {
            return Ok(());
        }
        if let Some(shell) = app.get_webview("shell") {
            shell
                .set_focus()
                .map_err(|error| format!("focus shell webview: {error}"))?;
        }
        Ok(())
    }

    pub fn activate(&mut self, app: &AppHandle, id: Uuid) -> Result<(), String> {
        if !self.tabs.contains_key(&id) {
            return Err("no such tab".to_string());
        }
        self.order.activate(id);
        if self
            .tabs
            .get(&id)
            .map(|tab| tab.webview.is_none())
            .unwrap_or(false)
        {
            self.ensure_webview(app, id)?;
        }
        self.apply_visibility()?;
        self.focus_shell_if_dashboard(app)?;
        self.announce_auth(app, id);
        Ok(())
    }

    pub fn activate_index(&mut self, app: &AppHandle, index: usize) -> Result<(), String> {
        self.order
            .activate_index(index)
            .ok_or_else(|| format!("no tab at index {index}"))?;
        if let Some(id) = self.order.active() {
            if self
                .tabs
                .get(&id)
                .map(|tab| tab.webview.is_none())
                .unwrap_or(false)
            {
                self.ensure_webview(app, id)?;
            }
            self.apply_visibility()?;
            self.focus_shell_if_dashboard(app)?;
            self.announce_auth(app, id);
        } else {
            self.apply_visibility()?;
        }
        Ok(())
    }

    pub fn cycle(&mut self, app: &AppHandle, delta: isize) -> Result<(), String> {
        self.order
            .cycle(delta)
            .ok_or_else(|| "no tabs are open".to_string())?;
        if let Some(id) = self.order.active() {
            if self
                .tabs
                .get(&id)
                .map(|tab| tab.webview.is_none())
                .unwrap_or(false)
            {
                self.ensure_webview(app, id)?;
            }
            self.apply_visibility()?;
            self.focus_shell_if_dashboard(app)?;
            self.announce_auth(app, id);
        } else {
            self.apply_visibility()?;
        }
        Ok(())
    }

    /// Put a tab back into sign-in because a probe proved its session dead. The
    /// page is hidden and the shell is asked for a token, exactly as when the
    /// cookie is missing. Returns whether the state changed.
    pub fn request_sign_in(&mut self, app: &AppHandle, id: Uuid) -> bool {
        let Some(tab) = self.tabs.get(&id) else {
            return false;
        };
        if tab.auth_required || tab.webview.is_none() {
            return false;
        }
        if let Some(tab) = self.tabs.get_mut(&id) {
            tab.auth_required = true;
        }
        let _ = self.apply_visibility();
        self.announce_auth(app, id);
        true
    }

    /// Ask the shell for a token for a tab that is waiting for one. Re-emitting
    /// when the tab is activated is what makes a cancelled panel recoverable.
    fn announce_auth(&self, app: &AppHandle, id: Uuid) {
        let Some(tab) = self.tabs.get(&id) else {
            return;
        };
        if !tab.auth_required {
            return;
        }
        let _ = app.emit(
            "auth://required",
            AuthRequiredEvent {
                instance_id: id,
                instance_name: tab.instance.name.clone(),
            },
        );
    }

    pub fn reload_active(&self) -> Result<(), String> {
        let id = self.order.active().ok_or("no active tab")?;
        let tab = self.tabs.get(&id).ok_or("no active tab")?;
        tab.webview
            .as_ref()
            .ok_or("tab is not loaded yet")?
            .reload()
            .map_err(|error| format!("reload tab: {error}"))
    }

    pub fn active_url(&self) -> Option<String> {
        let id = self.order.active()?;
        let tab = self.tabs.get(&id)?;
        if tab.instance.is_dashboard() {
            // There is no page to open; the service's own JSON is the closest
            // thing to "open this in a browser" and is useful for debugging.
            return crate::model::healthz_url(&tab.instance.url)
                .ok()
                .map(|url| url.to_string());
        }
        tab.webview
            .as_ref()
            .and_then(|webview| webview.url().ok())
            .map(|url| url.to_string())
            .or_else(|| Some(tab.url.clone()))
    }

    /// Close the active tab. Returns false when there was no tab to close, so
    /// the caller can decide to close the window instead.
    pub fn close_active(&mut self, app: &AppHandle) -> Result<bool, String> {
        let Some(id) = self.order.active() else {
            return Ok(false);
        };
        self.close(id)?;
        if self.order.active().is_none() {
            if let Some(shell) = app.get_webview("shell") {
                shell
                    .set_focus()
                    .map_err(|error| format!("focus shell webview: {error}"))?;
            }
        }
        Ok(true)
    }

    pub fn close_all(&mut self) {
        for (_, tab) in self.tabs.drain() {
            if let Some(webview) = tab.webview {
                let _ = webview.close();
            }
        }
        self.order.clear();
    }

    pub fn rename(&mut self, id: Uuid, name: &str) {
        if let Some(tab) = self.tabs.get_mut(&id) {
            tab.instance.name = name.to_string();
        }
    }

    pub fn begin_auth(&mut self, id: Uuid, target: Url) -> Result<(), String> {
        let tab = self.tabs.get_mut(&id).ok_or("no such tab")?;
        tab.auth_required = true;
        let webview = tab.webview.as_ref().ok_or("tab is not loaded yet")?;
        webview
            .navigate(target)
            .map_err(|error| format!("navigate to the token URL: {error}"))?;
        webview
            .hide()
            .map_err(|error| format!("hide tab during sign-in: {error}"))
    }

    /// The loaded content webviews with the instance id and base URL they belong
    /// to. The caller must read cookies after this lock is released:
    /// `Webview::cookies()` blocks on the main thread, which can be waiting for
    /// this mutex.
    pub fn loaded_webviews(&self) -> Vec<(Uuid, Webview, Url)> {
        self.tabs
            .iter()
            .filter_map(|(id, tab)| {
                let webview = tab.webview.clone()?;
                let url = Url::parse(&tab.url).ok()?;
                Some((*id, webview, url))
            })
            .collect()
    }

    /// `(url still carries a token, dsh cookie is present)`. The poll loop needs
    /// both: a successful exchange removes the token and sets the cookie.
    pub fn auth_status(&self, id: Uuid) -> Option<(bool, bool)> {
        let tab = self.tabs.get(&id)?;
        let webview = tab.webview.as_ref()?;
        let url = webview.url().ok()?;
        let has_token = url.query_pairs().any(|(key, _)| key == "token");
        let expected = auth::cookie_name(&Url::parse(&tab.url).ok()?);
        let has_cookie = webview
            .cookies()
            .ok()
            .map(|cookies| {
                cookies
                    .iter()
                    .any(|cookie| cookie.name() == expected.as_str())
            })
            .unwrap_or(false);
        Some((has_token, has_cookie))
    }

    pub fn complete_auth(&mut self, id: Uuid) -> Result<(), String> {
        let tab = self.tabs.get_mut(&id).ok_or("no such tab")?;
        tab.auth_required = false;
        self.apply_visibility()
    }

    pub fn hide_content(&mut self, app: &AppHandle) -> Result<(), String> {
        if let Some(id) = self.order.active() {
            if let Some(webview) = self.tabs.get(&id).and_then(|tab| tab.webview.as_ref()) {
                webview
                    .hide()
                    .map_err(|error| format!("hide tab webview: {error}"))?;
            }
        }
        if let Some(shell) = app.get_webview("shell") {
            shell
                .set_focus()
                .map_err(|error| format!("focus shell webview: {error}"))?;
        }
        Ok(())
    }

    pub fn show_content(&mut self) -> Result<(), String> {
        self.apply_visibility()
    }

    pub fn list(&self) -> Vec<TabInfo> {
        self.order
            .ids()
            .iter()
            .filter_map(|id| self.tabs.get(id).map(|tab| self.to_info(*id, tab)))
            .collect()
    }

    fn info(&self, id: Uuid) -> Option<TabInfo> {
        self.tabs.get(&id).map(|tab| self.to_info(id, tab))
    }

    fn to_info(&self, id: Uuid, tab: &Tab) -> TabInfo {
        TabInfo {
            instance_id: id,
            instance_name: tab.instance.name.clone(),
            role: tab.instance.role(),
            title: tab.title.clone(),
            url: tab.url.clone(),
            loading: tab.loading,
            auth_required: tab.auth_required,
            loaded: tab.webview.is_some() || tab.instance.is_dashboard(),
            active: self.order.active() == Some(id),
        }
    }

    fn apply_bounds(&self) -> Result<(), String> {
        let Some(bounds) = self.bounds else {
            return Ok(());
        };
        for tab in self.tabs.values() {
            let Some(webview) = tab.webview.as_ref() else {
                continue;
            };
            webview
                .set_position(LogicalPosition::new(bounds.x, bounds.y))
                .map_err(|error| format!("position tab webview: {error}"))?;
            webview
                .set_size(LogicalSize::new(bounds.width, bounds.height))
                .map_err(|error| format!("size tab webview: {error}"))?;
        }
        Ok(())
    }

    fn apply_visibility(&self) -> Result<(), String> {
        let active = self.order.active();
        for (id, tab) in &self.tabs {
            let Some(webview) = tab.webview.as_ref() else {
                continue;
            };
            if Some(*id) == active && !tab.auth_required {
                webview
                    .show()
                    .map_err(|error| format!("show tab webview: {error}"))?;
                webview
                    .set_focus()
                    .map_err(|error| format!("focus tab webview: {error}"))?;
            } else {
                webview
                    .hide()
                    .map_err(|error| format!("hide tab webview: {error}"))?;
            }
        }
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::model::{InstanceInput, InstanceKind, Role};

    fn id() -> Uuid {
        Uuid::new_v4()
    }

    fn instance(name: &str, kind: InstanceKind, url: &str) -> Instance {
        Instance::from_input(
            &InstanceInput {
                name: name.to_string(),
                kind,
                url: url.to_string(),
                group: None,
                notes: None,
            },
            0,
        )
        .expect("valid instance")
    }

    #[test]
    fn opening_a_tab_activates_it_and_a_duplicate_only_activates() {
        let mut order = TabOrder::default();
        let first = id();
        let second = id();
        assert!(order.open(first));
        assert!(order.open(second));
        assert_eq!(order.active(), Some(second));
        assert!(!order.open(first));
        assert_eq!(order.active(), Some(first));
    }

    #[test]
    fn closing_the_active_tab_selects_the_right_neighbour() {
        let mut order = TabOrder::default();
        let first = id();
        let second = id();
        let third = id();
        order.open(first);
        order.open(second);
        order.open(third);
        order.activate(second);
        assert_eq!(order.close(second), Some(third));
        assert_eq!(order.active(), Some(third));
    }

    #[test]
    fn closing_the_last_active_tab_falls_back_and_an_empty_order_clears_it() {
        let mut order = TabOrder::default();
        let first = id();
        let second = id();
        order.open(first);
        order.open(second);
        assert_eq!(order.close(second), Some(first));
        assert_eq!(order.close(first), None);
        assert_eq!(order.active(), None);
    }

    #[test]
    fn closing_a_background_tab_keeps_the_active_one() {
        let mut order = TabOrder::default();
        let first = id();
        let second = id();
        order.open(first);
        order.open(second);
        assert_eq!(order.close(first), Some(second));
        assert_eq!(order.active(), Some(second));
    }

    #[test]
    fn cycling_wraps_in_both_directions() {
        let mut order = TabOrder::default();
        let first = id();
        let second = id();
        let third = id();
        order.open(first);
        order.open(second);
        order.open(third);
        assert_eq!(order.cycle(1), Some(first));
        assert_eq!(order.cycle(1), Some(second));
        assert_eq!(order.cycle(-1), Some(first));
        assert_eq!(order.cycle(-1), Some(third));
    }

    #[test]
    fn activating_by_index_rejects_out_of_range() {
        let mut order = TabOrder::default();
        let first = id();
        order.open(first);
        assert_eq!(order.activate_index(0), Some(first));
        assert_eq!(order.activate_index(1), None);
    }

    #[test]
    fn viewport_top_inset_is_the_title_bar_gap_and_never_negative() {
        assert_eq!(viewport_top_inset(800.0, 768.0), 32.0);
        assert_eq!(viewport_top_inset(800.0, 800.0), 0.0);
        assert_eq!(viewport_top_inset(800.0, 820.0), 0.0);
    }

    #[test]
    fn stage_bounds_reject_offscreen_or_empty_rectangles() {
        assert!(StageBounds::new(0.0, 38.0, 1000.0, 700.0).is_ok());
        assert!(StageBounds::new(-1.0, 0.0, 100.0, 100.0).is_err());
        assert!(StageBounds::new(0.0, 0.0, 0.0, 100.0).is_err());
        assert!(StageBounds::new(0.0, 0.0, f64::NAN, 100.0).is_err());
    }

    #[test]
    fn tab_labels_never_match_the_shell() {
        let label = tab_label(id());
        assert!(label.starts_with("tab-"));
        assert_ne!(label, "shell");
    }

    #[test]
    fn capability_grants_only_the_shell() {
        let raw = include_str!("../capabilities/shell.json");
        let value: serde_json::Value = serde_json::from_str(raw).expect("valid capability json");
        assert_eq!(value["webviews"], serde_json::json!(["shell"]));
        assert!(value.get("windows").is_none());
        assert!(value.get("remote").is_none());
    }

    #[test]
    fn restore_keeps_order_and_creates_placeholders() {
        let first = instance("one", InstanceKind::Other, "http://127.0.0.1:1");
        let second = instance("two", InstanceKind::Other, "http://127.0.0.1:2");
        let manager = TabManager::restore(
            &[first.clone(), second.clone()],
            &[second.id, first.id],
            Some(first.id),
        );
        let tabs = manager.list();
        assert_eq!(tabs.len(), 2);
        assert_eq!(tabs[0].instance_id, second.id);
        assert_eq!(tabs[1].instance_id, first.id);
        assert!(tabs.iter().all(|tab| !tab.loaded));
        assert!(tabs.iter().all(|tab| tab.title.is_empty()));
        assert_eq!(
            manager.session_snapshot(),
            (vec![second.id, first.id], Some(first.id))
        );
    }

    #[test]
    fn restored_tabs_carry_the_role_of_their_instance() {
        let tracker = instance(
            "yt",
            InstanceKind::Youtrack,
            "https://youtrack.example.com/",
        );
        let agent = instance("oc", InstanceKind::Opencode, "http://127.0.0.1:4096");
        let manager = TabManager::restore(
            &[tracker.clone(), agent.clone()],
            &[tracker.id, agent.id],
            None,
        );
        let tabs = manager.list();
        assert_eq!(tabs[0].role, Role::Upstream);
        assert_eq!(tabs[1].role, Role::Downstream);
    }

    #[test]
    fn a_hub_tab_is_loaded_without_a_webview() {
        let hub = instance("hub", InstanceKind::FlowHub, "http://127.0.0.1:8080/");
        let manager = TabManager::restore(std::slice::from_ref(&hub), &[hub.id], Some(hub.id));
        let tabs = manager.list();
        assert_eq!(tabs.len(), 1);
        assert!(tabs[0].loaded);
        assert_eq!(tabs[0].role, Role::Hub);
        assert!(manager.active_is_dashboard());
    }

    #[test]
    fn restore_skips_ids_without_an_instance() {
        let first = instance("one", InstanceKind::Other, "http://127.0.0.1:1");
        let missing = id();
        let manager = TabManager::restore(std::slice::from_ref(&first), &[missing, first.id], None);
        let tabs = manager.list();
        assert_eq!(tabs.len(), 1);
        assert_eq!(tabs[0].instance_id, first.id);
        assert!(tabs[0].active);
    }
}
