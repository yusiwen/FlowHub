// A plain window hosts two kinds of webview: the local shell (sidebar and tab
// bar) and one remote child webview per instance. The shell is the only webview
// in a capability; remote pages must never reach the IPC layer.
mod auth;
mod commands;
mod health;
mod hub;
mod hub_api;
mod ime;
mod menu;
mod model;
mod prefs;
mod session;
mod state;
mod store;
mod tabs;
mod token_store;

use tauri::webview::WebviewBuilder;
use tauri::window::WindowBuilder;
use tauri::{LogicalPosition, Manager, WebviewUrl};

fn main() {
    tauri::Builder::default()
        .on_menu_event(|app, event| menu::handle(app, event.id().as_ref()))
        .setup(|app| {
            let config_dir = app.path().app_config_dir()?;
            // The app was called "Agent Hub" under a `dev.flowhub.agenthub`
            // identifier until the rename to FlowHub; keep the registry it left
            // behind instead of starting empty.
            if let Err(error) = state::migrate_legacy_config(&config_dir) {
                eprintln!("migrate the Agent Hub registry: {error}");
            }
            app.manage(state::AppState::load(&config_dir)?);

            let probe_app = app.handle().clone();
            tauri::async_runtime::spawn(async move {
                loop {
                    health::probe_all(&probe_app).await;
                    tokio::time::sleep(std::time::Duration::from_secs(30)).await;
                }
            });

            let window = WindowBuilder::new(app, "main")
                .title("FlowHub")
                .inner_size(1280.0, 800.0)
                .min_inner_size(900.0, 600.0)
                .build()?;

            let shell = WebviewBuilder::new("shell", WebviewUrl::App("index.html".into()))
                .initialization_script(ime::SCRIPT)
                .auto_resize();

            window.add_child(shell, LogicalPosition::new(0.0, 0.0), window.inner_size()?)?;
            menu::install(app)?;
            menu::watch_window(&window);

            Ok(())
        })
        .invoke_handler(tauri::generate_handler![
            commands::list_instances,
            commands::add_instance,
            commands::update_instance,
            commands::remove_instance,
            commands::reorder_instances,
            commands::list_tabs,
            commands::open_tab,
            commands::close_tab,
            commands::activate_tab,
            commands::set_stage_bounds,
            commands::set_content_visible,
            commands::submit_token,
            commands::list_health,
            commands::probe_all,
            commands::hub_snapshot,
            commands::hub_flows,
            commands::hub_flow,
            commands::hub_runs,
            commands::hub_run,
            commands::set_hub_token,
            commands::hub_logs,
            commands::set_hub_log_path,
            commands::load_session,
            commands::set_sidebar_visible,
        ])
        .run(tauri::generate_context!())
        .expect("error while running FlowHub");
}
