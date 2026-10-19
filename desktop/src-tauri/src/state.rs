use crate::store::{Store, StoreError};
use std::path::Path;
use std::sync::Mutex;

/// Tauri managed state. One process-wide registry lock is enough: the shell
/// issues commands one at a time and each mutation rewrites the whole file.
pub struct AppState {
    pub store: Mutex<Store>,
}

impl AppState {
    pub fn load(dir: &Path) -> Result<Self, StoreError> {
        Ok(Self {
            store: Mutex::new(Store::load(dir)?),
        })
    }
}
