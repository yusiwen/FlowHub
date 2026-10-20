//! The macOS menu owns the accelerators: a focused content webview receives
//! keyboard events, so shell key handlers never see Cmd+T/W/R/B. The Edit menu
//! is also what makes Cmd+C/V work inside a webview.

use crate::commands;
use crate::state::AppState;
use tauri::menu::{Menu, MenuItem, PredefinedMenuItem, Submenu};
use tauri::{App, AppHandle, Emitter, Manager, Window, WindowEvent};

pub fn build(app: &App) -> tauri::Result<Menu<tauri::Wry>> {
    let app_menu = Submenu::with_items(
        app,
        "FlowHub",
        true,
        &[
            &PredefinedMenuItem::about(app, Some("About FlowHub"), None)?,
            &PredefinedMenuItem::separator(app)?,
            &PredefinedMenuItem::hide(app, None)?,
            &PredefinedMenuItem::hide_others(app, None)?,
            &PredefinedMenuItem::show_all(app, None)?,
            &PredefinedMenuItem::separator(app)?,
            &PredefinedMenuItem::quit(app, None)?,
        ],
    )?;

    let file_menu = Submenu::with_items(
        app,
        "File",
        true,
        &[
            &MenuItem::with_id(app, "new-tab", "New Tab", true, Some("CmdOrCtrl+T"))?,
            &MenuItem::with_id(app, "close-tab", "Close Tab", true, Some("CmdOrCtrl+W"))?,
            &MenuItem::with_id(app, "reload-tab", "Reload", true, Some("CmdOrCtrl+R"))?,
            &MenuItem::with_id(
                app,
                "open-browser",
                "Open in Browser",
                true,
                Some("CmdOrCtrl+Shift+O"),
            )?,
            &PredefinedMenuItem::separator(app)?,
            &MenuItem::with_id(
                app,
                "close-window",
                "Close Window",
                true,
                Some("CmdOrCtrl+Shift+W"),
            )?,
        ],
    )?;

    let edit_menu = Submenu::with_items(
        app,
        "Edit",
        true,
        &[
            &PredefinedMenuItem::undo(app, None)?,
            &PredefinedMenuItem::redo(app, None)?,
            &PredefinedMenuItem::separator(app)?,
            &PredefinedMenuItem::cut(app, None)?,
            &PredefinedMenuItem::copy(app, None)?,
            &PredefinedMenuItem::paste(app, None)?,
            &PredefinedMenuItem::select_all(app, None)?,
        ],
    )?;

    let view_menu = Submenu::with_items(
        app,
        "View",
        true,
        &[
            &MenuItem::with_id(
                app,
                "scope-upstream",
                "Upstream",
                true,
                Some("CmdOrCtrl+Shift+1"),
            )?,
            &MenuItem::with_id(
                app,
                "scope-instances",
                "Instances",
                true,
                Some("CmdOrCtrl+Shift+2"),
            )?,
            &PredefinedMenuItem::separator(app)?,
            &MenuItem::with_id(
                app,
                "toggle-sidebar",
                "Toggle Sidebar",
                true,
                Some("CmdOrCtrl+B"),
            )?,
        ],
    )?;

    let next = MenuItem::with_id(app, "next-tab", "Next Tab", true, Some("Ctrl+Tab"))?;
    let previous = MenuItem::with_id(
        app,
        "prev-tab",
        "Previous Tab",
        true,
        Some("Ctrl+Shift+Tab"),
    )?;
    let tab_separator = PredefinedMenuItem::separator(app)?;
    let tab1 = MenuItem::with_id(app, "tab-1", "Tab 1", true, Some("CmdOrCtrl+1"))?;
    let tab2 = MenuItem::with_id(app, "tab-2", "Tab 2", true, Some("CmdOrCtrl+2"))?;
    let tab3 = MenuItem::with_id(app, "tab-3", "Tab 3", true, Some("CmdOrCtrl+3"))?;
    let tab4 = MenuItem::with_id(app, "tab-4", "Tab 4", true, Some("CmdOrCtrl+4"))?;
    let tab5 = MenuItem::with_id(app, "tab-5", "Tab 5", true, Some("CmdOrCtrl+5"))?;
    let tab6 = MenuItem::with_id(app, "tab-6", "Tab 6", true, Some("CmdOrCtrl+6"))?;
    let tab7 = MenuItem::with_id(app, "tab-7", "Tab 7", true, Some("CmdOrCtrl+7"))?;
    let tab8 = MenuItem::with_id(app, "tab-8", "Tab 8", true, Some("CmdOrCtrl+8"))?;
    let tab9 = MenuItem::with_id(app, "tab-9", "Tab 9", true, Some("CmdOrCtrl+9"))?;

    let tab_menu = Submenu::with_items(
        app,
        "Tab",
        true,
        &[
            &next,
            &previous,
            &tab_separator,
            &tab1,
            &tab2,
            &tab3,
            &tab4,
            &tab5,
            &tab6,
            &tab7,
            &tab8,
            &tab9,
        ],
    )?;

    let window_menu = Submenu::with_items(
        app,
        "Window",
        true,
        &[&PredefinedMenuItem::minimize(app, None)?],
    )?;

    Menu::with_items(
        app,
        &[
            &app_menu,
            &file_menu,
            &edit_menu,
            &view_menu,
            &tab_menu,
            &window_menu,
        ],
    )
}

pub fn install(app: &mut App) -> tauri::Result<()> {
    let menu = build(app)?;
    app.set_menu(menu)?;
    Ok(())
}

fn close_window(app: &AppHandle) -> Result<(), String> {
    app.get_window("main")
        .ok_or_else(|| "main window is missing".to_string())?
        .close()
        .map_err(|error| format!("close window: {error}"))
}

pub fn handle(app: &AppHandle, id: &str) {
    let state = app.state::<AppState>();
    let result = match id {
        "new-tab" => app
            .emit("menu://new-tab", ())
            .map_err(|error| error.to_string()),
        "close-tab" => match commands::close_active(app, &state) {
            Ok(true) => Ok(()),
            Ok(false) => close_window(app),
            Err(error) => Err(error),
        },
        "reload-tab" => commands::reload_active(&state),
        "open-browser" => commands::open_in_browser(&state),
        "scope-upstream" => app
            .emit("scope://select", "upstream")
            .map_err(|error| error.to_string()),
        "scope-instances" => app
            .emit("scope://select", "instances")
            .map_err(|error| error.to_string()),
        "toggle-sidebar" => app
            .emit("menu://toggle-sidebar", ())
            .map_err(|error| error.to_string()),
        "next-tab" => commands::cycle_tab(app, &state, 1),
        "prev-tab" => commands::cycle_tab(app, &state, -1),
        "close-window" => close_window(app),
        other => match other
            .strip_prefix("tab-")
            .and_then(|value| value.parse::<usize>().ok())
        {
            Some(index) if index >= 1 => commands::activate_index(app, &state, index - 1),
            _ => Ok(()),
        },
    };
    if let Err(error) = result {
        eprintln!("menu {id}: {error}");
    }
}

/// Closing the window closes every child webview before the process exits.
pub fn watch_window(window: &Window) {
    let app = window.app_handle().clone();
    window.on_window_event(move |event| {
        if matches!(event, WindowEvent::CloseRequested { .. }) {
            if let Ok(mut tabs) = app.state::<AppState>().tabs.lock() {
                tabs.close_all();
            }
        }
    });
}
