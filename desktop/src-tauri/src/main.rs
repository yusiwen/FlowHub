// A plain window hosts two kinds of webview: the local shell (sidebar and tab
// bar) and, later, one remote child webview per instance. The shell is the only
// webview in a capability; remote pages must never reach the IPC layer.
use tauri::webview::WebviewBuilder;
use tauri::window::WindowBuilder;
use tauri::{LogicalPosition, WebviewUrl};

fn main() {
    tauri::Builder::default()
        .setup(|app| {
            let window = WindowBuilder::new(app, "main")
                .title("Agent Hub")
                .inner_size(1280.0, 800.0)
                .min_inner_size(900.0, 600.0)
                .build()?;

            let shell =
                WebviewBuilder::new("shell", WebviewUrl::App("index.html".into())).auto_resize();

            window.add_child(shell, LogicalPosition::new(0.0, 0.0), window.inner_size()?)?;

            Ok(())
        })
        .run(tauri::generate_context!())
        .expect("error while running Agent Hub");
}
