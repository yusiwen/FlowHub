use crate::health::HealthReport;
use crate::session::SessionStore;
use crate::store::{Store, StoreError};
use crate::tabs::TabManager;
use std::collections::{HashMap, HashSet};
use std::path::Path;
use std::sync::atomic::AtomicBool;
use std::sync::{Mutex, MutexGuard};
use uuid::Uuid;

/// Tauri managed state. The registry is rewritten as a whole on every mutation;
/// the tab manager owns the native child webviews and the stage rectangle; the
/// session file holds the restored tab ids.
pub struct AppState {
    pub store: Mutex<Store>,
    pub tabs: Mutex<TabManager>,
    pub session: Mutex<SessionStore>,
    pub health: Mutex<HashMap<Uuid, HealthReport>>,
    pub probing: AtomicBool,
}

impl AppState {
    pub fn load(dir: &Path) -> Result<Self, StoreError> {
        let store = Store::load(dir)?;
        let mut session = SessionStore::load(dir);

        let valid: HashSet<Uuid> = store
            .instances()
            .iter()
            .map(|instance| instance.id)
            .collect();
        let pruned = session.prune(&valid);
        let ids = session.tabs().to_vec();
        let active = session.active();
        let tabs = TabManager::restore(store.instances(), &ids, active);
        let (restored_tabs, restored_active) = tabs.session_snapshot();

        if session.needs_save() || pruned || restored_tabs != ids || restored_active != active {
            let sidebar = session.sidebar_visible();
            if let Err(error) = session.save(sidebar, restored_tabs, restored_active) {
                eprintln!("session: {error}");
            }
        }

        Ok(Self {
            store: Mutex::new(store),
            tabs: Mutex::new(tabs),
            session: Mutex::new(session),
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

pub fn lock_session<'a>(state: &'a AppState) -> Result<MutexGuard<'a, SessionStore>, String> {
    state
        .session
        .lock()
        .map_err(|_| "session lock poisoned".to_string())
}
