use crate::health::HealthReport;
use crate::prefs::PrefsStore;
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
    pub prefs: Mutex<PrefsStore>,
    pub health: Mutex<HashMap<Uuid, HealthReport>>,
    pub probing: AtomicBool,
}

impl AppState {
    pub fn load(dir: &Path) -> Result<Self, StoreError> {
        let store = Store::load(dir)?;
        let prefs = PrefsStore::load(dir);
        if prefs.needs_save() {
            if let Err(error) = prefs.save() {
                eprintln!("prefs: {error}");
            }
        }
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
            prefs: Mutex::new(prefs),
            health: Mutex::new(HashMap::new()),
            probing: AtomicBool::new(false),
        })
    }
}

pub fn lock_prefs<'a>(state: &'a AppState) -> Result<MutexGuard<'a, PrefsStore>, String> {
    state
        .prefs
        .lock()
        .map_err(|_| "preferences lock poisoned".to_string())
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

/// The identifier the app used while it was called "Agent Hub".
const LEGACY_APP_DIR: &str = "dev.flowhub.agenthub";

/// The files worth carrying across the rename to `cn.yusiwen.flowhub`.
const LEGACY_FILES: [&str; 2] = ["instances.json", "session.json"];

/// Copy the registry and the session out of the legacy config directory.
///
/// The config directory is named after the bundle identifier, so the rename to
/// `cn.yusiwen.flowhub` starts the app in an empty directory and the user's
/// instances would look lost. Each file is copied only when the new directory
/// does not already have it, which makes a second start a no-op instead of an
/// overwrite. The legacy directory is left in place: nothing here may delete a
/// user's data.
pub fn migrate_legacy_config(dir: &Path) -> std::io::Result<()> {
    let Some(parent) = dir.parent() else {
        return Ok(());
    };
    let legacy = parent.join(LEGACY_APP_DIR);
    if !legacy.is_dir() {
        return Ok(());
    }

    for name in LEGACY_FILES {
        let from = legacy.join(name);
        let to = dir.join(name);
        if to.exists() || !from.is_file() {
            continue;
        }
        std::fs::create_dir_all(dir)?;
        std::fs::copy(&from, &to)?;
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::fs;

    #[test]
    fn the_legacy_registry_is_copied_once() {
        let root = tempfile::tempdir().expect("tempdir");
        let legacy = root.path().join(LEGACY_APP_DIR);
        fs::create_dir_all(&legacy).expect("legacy dir");
        fs::write(legacy.join("instances.json"), "{\"version\":1}").expect("instances");
        fs::write(legacy.join("session.json"), "{\"version\":1}").expect("session");

        let dir = root.path().join("cn.yusiwen.flowhub");
        migrate_legacy_config(&dir).expect("migrate");
        assert_eq!(
            fs::read_to_string(dir.join("instances.json")).expect("read"),
            "{\"version\":1}"
        );
        assert_eq!(
            fs::read_to_string(dir.join("session.json")).expect("read"),
            "{\"version\":1}"
        );

        // A later start must not replace what the app has written since.
        fs::write(dir.join("instances.json"), "{\"version\":2}").expect("write");
        migrate_legacy_config(&dir).expect("migrate again");
        assert_eq!(
            fs::read_to_string(dir.join("instances.json")).expect("read"),
            "{\"version\":2}"
        );
    }

    #[test]
    fn a_missing_legacy_directory_is_a_no_op() {
        let root = tempfile::tempdir().expect("tempdir");
        let dir = root.path().join("cn.yusiwen.flowhub");
        migrate_legacy_config(&dir).expect("migrate");
        assert!(!dir.exists());
    }
}
