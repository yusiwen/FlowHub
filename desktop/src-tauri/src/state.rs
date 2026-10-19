use crate::store::{Store, StoreError};
use crate::tabs::TabManager;
use std::path::Path;
use std::sync::{Mutex, MutexGuard};

/// Tauri managed state. The registry is rewritten as a whole on every mutation;
/// the tab manager owns the native child webviews and the stage rectangle.
pub struct AppState {
    pub store: Mutex<Store>,
    pub tabs: Mutex<TabManager>,
}

impl AppState {
    pub fn load(dir: &Path) -> Result<Self, StoreError> {
        Ok(Self {
            store: Mutex::new(Store::load(dir)?),
            tabs: Mutex::new(TabManager::default()),
        })
    }
}

pub fn lock_tabs<'a>(state: &'a AppState) -> Result<MutexGuard<'a, TabManager>, String> {
    state
        .tabs
        .lock()
        .map_err(|_| "tab manager lock poisoned".to_string())
}
