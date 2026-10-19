//! One native child WebView per instance tab.
//!
//! Content webviews are attached to the same plain window as the shell but are
//! not named in any capability, so a remote page has no Tauri IPC. The shell
//! owns the sidebar and tab bar; Rust owns the stage rectangle and applies it to
//! every child webview.

use crate::model::Instance;
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
    pub title: String,
    pub url: String,
    pub loading: bool,
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
}

fn tab_label(id: Uuid) -> String {
    format!("tab-{id}")
}

struct Tab {
    instance_name: String,
    title: String,
    url: String,
    loading: bool,
    webview: Webview,
}

#[derive(Default)]
pub struct TabManager {
    tabs: HashMap<Uuid, Tab>,
    order: TabOrder,
    bounds: Option<StageBounds>,
}

impl TabManager {
    pub fn set_bounds(&mut self, bounds: StageBounds) -> Result<(), String> {
        self.bounds = Some(bounds);
        self.apply_bounds()?;
        self.apply_visibility()
    }

    pub fn open(&mut self, app: &AppHandle, instance: &Instance) -> Result<TabInfo, String> {
        if self.tabs.contains_key(&instance.id) {
            self.order.activate(instance.id);
            self.apply_visibility()?;
            return Ok(self.info(instance.id).ok_or("tab disappeared")?);
        }

        let bounds = self
            .bounds
            .ok_or_else(|| "stage bounds are not set yet".to_string())?;
        let window = app
            .get_window("main")
            .ok_or_else(|| "main window is missing".to_string())?;
        let url =
            Url::parse(&instance.url).map_err(|error| format!("invalid instance url: {error}"))?;
        let instance_id = instance.id;
        let label = tab_label(instance_id);

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

        self.tabs.insert(
            instance.id,
            Tab {
                instance_name: instance.name.clone(),
                title: instance.name.clone(),
                url: instance.url.clone(),
                loading: false,
                webview,
            },
        );
        self.order.open(instance.id);
        self.apply_visibility()?;
        self.info(instance.id).ok_or("tab disappeared".to_string())
    }

    pub fn close(&mut self, id: Uuid) -> Result<(), String> {
        if let Some(tab) = self.tabs.remove(&id) {
            tab.webview
                .close()
                .map_err(|error| format!("close tab webview: {error}"))?;
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

    pub fn activate(&mut self, id: Uuid) -> Result<(), String> {
        if !self.order.activate(id) {
            return Err("no such tab".to_string());
        }
        self.apply_visibility()
    }

    pub fn rename(&mut self, id: Uuid, name: &str) {
        if let Some(tab) = self.tabs.get_mut(&id) {
            tab.instance_name = name.to_string();
        }
    }

    pub fn hide_content(&mut self, app: &AppHandle) -> Result<(), String> {
        if let Some(id) = self.order.active() {
            if let Some(tab) = self.tabs.get(&id) {
                tab.webview
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
        let active = self.order.active();
        self.order
            .ids()
            .iter()
            .filter_map(|id| {
                self.tabs.get(id).map(|tab| TabInfo {
                    instance_id: *id,
                    instance_name: tab.instance_name.clone(),
                    title: tab.title.clone(),
                    url: tab.url.clone(),
                    loading: tab.loading,
                    active: Some(*id) == active,
                })
            })
            .collect()
    }

    fn info(&self, id: Uuid) -> Option<TabInfo> {
        let active = self.order.active();
        self.tabs.get(&id).map(|tab| TabInfo {
            instance_id: id,
            instance_name: tab.instance_name.clone(),
            title: tab.title.clone(),
            url: tab.url.clone(),
            loading: tab.loading,
            active: Some(id) == active,
        })
    }

    fn apply_bounds(&self) -> Result<(), String> {
        let Some(bounds) = self.bounds else {
            return Ok(());
        };
        for tab in self.tabs.values() {
            tab.webview
                .set_position(LogicalPosition::new(bounds.x, bounds.y))
                .map_err(|error| format!("position tab webview: {error}"))?;
            tab.webview
                .set_size(LogicalSize::new(bounds.width, bounds.height))
                .map_err(|error| format!("size tab webview: {error}"))?;
        }
        Ok(())
    }

    fn apply_visibility(&self) -> Result<(), String> {
        let active = self.order.active();
        for (id, tab) in &self.tabs {
            if Some(*id) == active {
                tab.webview
                    .show()
                    .map_err(|error| format!("show tab webview: {error}"))?;
                tab.webview
                    .set_focus()
                    .map_err(|error| format!("focus tab webview: {error}"))?;
            } else {
                tab.webview
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

    fn id() -> Uuid {
        Uuid::new_v4()
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
}
