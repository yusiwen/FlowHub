use crate::health::HealthReport;
use crate::store::{Store, StoreError};
use crate::tabs::TabManager;
use std::collections::HashMap;
use std::path::Path;
use std::sync::atomic::AtomicBool;
use std::sync::{Mutex, MutexGuard};
use uuid::Uuid;

/// Tauri managed state. The registry is rewritten as a whole on every mutation;
/// the tab manager owns the native child webviews and the stage rectangle.
pub struct AppState {
    pub store: Mutex<Store>,
    pub tabs: Mutex<TabManager>,
    pub health: Mutex<HashMap<Uuid, HealthReport>>,
    pub probing: AtomicBool,
}

impl AppState {
    pub fn load(dir: &Path) -> Result<Self, StoreError> {
        Ok(Self {
            store: Mutex::new(Store::load(dir)?),
            tabs: Mutex::new(TabManager::default()),
            health: Mutex::new(HashMap::new()),
            probing: AtomicBool::new(false),
        })
    }
}

pub fn lock_tabs<'a>(state: &'a AppState) -> Result<MutexGuard<'a, TabManager>, String> {
    state
        .tabs
        .lock()
        .map_err(|_| "tab manager lock poisoned".to_string())
}
