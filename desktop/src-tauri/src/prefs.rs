//! Preferences that belong to this machine rather than to a service.
//!
//! Today the only entry is the confirmed log path for a Hub whose address is not
//! loopback. A path the service reports is displayed, never opened, until the
//! user chooses it here: a configured URL must not become an arbitrary-file-read
//! primitive.
//!
//! The file holds no secret, so it is `0644` (unlike the registry and the
//! credentials files). A corrupt or unknown-version file starts empty and is
//! rewritten instead of blocking the app.

use serde::{Deserialize, Serialize};
use std::collections::HashMap;
use std::fs::{self, OpenOptions};
use std::io::Write;
use std::os::unix::fs::OpenOptionsExt;
use std::path::{Path, PathBuf};
use uuid::Uuid;

pub const PREFS_VERSION: u32 = 1;
const PREFS_FILE: &str = "prefs.json";

#[derive(Debug, Serialize, Deserialize)]
struct PrefsFile {
    version: u32,
    #[serde(default)]
    hub_log_paths: HashMap<Uuid, String>,
}

fn empty() -> PrefsFile {
    PrefsFile {
        version: PREFS_VERSION,
        hub_log_paths: HashMap::new(),
    }
}

#[derive(Debug)]
pub struct PrefsStore {
    path: PathBuf,
    data: PrefsFile,
    needs_save: bool,
}

impl PrefsStore {
    pub fn load(dir: &Path) -> Self {
        let path = dir.join(PREFS_FILE);
        let mut needs_save = false;
        let data = if path.exists() {
            match fs::read_to_string(&path) {
                Ok(raw) => match serde_json::from_str::<PrefsFile>(&raw) {
                    Ok(file) if file.version == PREFS_VERSION => file,
                    Ok(file) => {
                        eprintln!(
                            "prefs: unsupported version {}; starting empty",
                            file.version
                        );
                        needs_save = true;
                        empty()
                    }
                    Err(error) => {
                        eprintln!("prefs: {error}; starting empty");
                        needs_save = true;
                        empty()
                    }
                },
                Err(error) => {
                    eprintln!("prefs: {error}; starting empty");
                    needs_save = true;
                    empty()
                }
            }
        } else {
            empty()
        };

        Self {
            path,
            data,
            needs_save,
        }
    }

    pub fn needs_save(&self) -> bool {
        self.needs_save
    }

    pub fn hub_log_path(&self, id: Uuid) -> Option<&str> {
        self.data.hub_log_paths.get(&id).map(String::as_str)
    }

    /// Persist the user's choice for one Hub. Clearing it removes the entry, so
    /// the resolution falls back to the loopback rule.
    pub fn set_hub_log_path(&mut self, id: Uuid, path: Option<&str>) -> Result<(), String> {
        match path {
            Some(path) => {
                self.data.hub_log_paths.insert(id, path.to_string());
            }
            None => {
                self.data.hub_log_paths.remove(&id);
            }
        }
        self.save()
    }

    /// Forget a removed instance. A stale path is harmless but would attach
    /// itself to a future instance that happens to reuse the id.
    pub fn remove(&mut self, id: Uuid) -> Result<(), String> {
        if self.data.hub_log_paths.remove(&id).is_some() {
            self.save()?;
        }
        Ok(())
    }

    pub fn save(&self) -> Result<(), String> {
        if let Some(parent) = self.path.parent() {
            fs::create_dir_all(parent)
                .map_err(|error| format!("create {}: {error}", parent.display()))?;
        }
        let bytes = serde_json::to_vec_pretty(&self.data)
            .map_err(|error| format!("encode {}: {error}", self.path.display()))?;

        let tmp = self.path.with_extension("json.tmp");
        {
            let mut out = OpenOptions::new()
                .write(true)
                .create(true)
                .truncate(true)
                .mode(0o644)
                .open(&tmp)
                .map_err(|error| format!("write {}: {error}", tmp.display()))?;
            out.write_all(&bytes)
                .map_err(|error| format!("write {}: {error}", tmp.display()))?;
            out.sync_all()
                .map_err(|error| format!("write {}: {error}", tmp.display()))?;
        }
        fs::rename(&tmp, &self.path)
            .map_err(|error| format!("write {}: {error}", self.path.display()))
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::os::unix::fs::PermissionsExt;

    fn temp_store() -> (tempfile::TempDir, PrefsStore) {
        let dir = tempfile::tempdir().expect("tempdir");
        let store = PrefsStore::load(dir.path());
        (dir, store)
    }

    #[test]
    fn a_path_round_trips_and_uses_0644() {
        let (dir, mut store) = temp_store();
        let id = Uuid::new_v4();
        store
            .set_hub_log_path(id, Some("/tmp/flowhub.log"))
            .expect("save");

        let path = dir.path().join(PREFS_FILE);
        let mode = fs::metadata(&path).expect("metadata").permissions().mode() & 0o777;
        assert_eq!(mode, 0o644);

        let reloaded = PrefsStore::load(dir.path());
        assert_eq!(reloaded.hub_log_path(id), Some("/tmp/flowhub.log"));
        assert!(!reloaded.needs_save());
    }

    #[test]
    fn clearing_a_path_removes_the_entry() {
        let (dir, mut store) = temp_store();
        let id = Uuid::new_v4();
        store
            .set_hub_log_path(id, Some("/tmp/flowhub.log"))
            .expect("save");
        store.set_hub_log_path(id, None).expect("clear");
        assert_eq!(store.hub_log_path(id), None);
        let raw = fs::read_to_string(dir.path().join(PREFS_FILE)).expect("read");
        assert!(!raw.contains("flowhub.log"), "{raw}");
    }

    #[test]
    fn removing_an_instance_forgets_its_path() {
        let (dir, mut store) = temp_store();
        let id = Uuid::new_v4();
        store
            .set_hub_log_path(id, Some("/tmp/flowhub.log"))
            .expect("save");
        store.remove(id).expect("remove");
        let reloaded = PrefsStore::load(dir.path());
        assert_eq!(reloaded.hub_log_path(id), None);
    }

    #[test]
    fn a_corrupt_file_starts_empty_and_asks_to_be_rewritten() {
        let dir = tempfile::tempdir().expect("tempdir");
        fs::write(dir.path().join(PREFS_FILE), "{").expect("write");
        let store = PrefsStore::load(dir.path());
        assert!(store.needs_save());
        assert_eq!(store.hub_log_path(Uuid::new_v4()), None);
    }

    #[test]
    fn an_unknown_version_starts_empty() {
        let dir = tempfile::tempdir().expect("tempdir");
        fs::write(
            dir.path().join(PREFS_FILE),
            r#"{"version":99,"hub_log_paths":{}}"#,
        )
        .expect("write");
        let store = PrefsStore::load(dir.path());
        assert!(store.needs_save());
    }
}
