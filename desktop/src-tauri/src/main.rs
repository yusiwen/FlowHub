// A plain window hosts two kinds of webview: the local shell (sidebar and tab
// bar) and one remote child webview per instance. The shell is the only webview
// in a capability; remote pages must never reach the IPC layer.
mod auth;
mod commands;
mod health;
mod menu;
mod model;
mod state;
mod store;
mod tabs;

use tauri::webview::WebviewBuilder;
use tauri::window::WindowBuilder;
use tauri::{LogicalPosition, Manager, WebviewUrl};

fn main() {
    tauri::Builder::default()
        .on_menu_event(|app, event| menu::handle(app, event.id().as_ref()))
        .setup(|app| {
            let config_dir = app.path().app_config_dir()?;
            app.manage(state::AppState::load(&config_dir)?);

            let probe_app = app.handle().clone();
            tauri::async_runtime::spawn(async move {
                loop {
                    health::probe_all(&probe_app).await;
                    tokio::time::sleep(std::time::Duration::from_secs(30)).await;
                }
            });

            let window = WindowBuilder::new(app, "main")
                .title("Agent Hub")
                .inner_size(1280.0, 800.0)
                .min_inner_size(900.0, 600.0)
                .build()?;

            let shell =
                WebviewBuilder::new("shell", WebviewUrl::App("index.html".into())).auto_resize();

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
        ])
        .run(tauri::generate_context!())
        .expect("error while running Agent Hub");
}
