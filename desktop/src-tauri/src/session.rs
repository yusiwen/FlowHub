//! The small session file: which tabs were open, which was active, and whether
//! the sidebar was visible. It holds instance ids only, so a removed instance
//! is dropped on the next load instead of resurrecting a dead tab.

use serde::{Deserialize, Serialize};
use std::collections::HashSet;
use std::fs::{self, OpenOptions};
use std::io::Write;
use std::os::unix::fs::OpenOptionsExt;
use std::path::{Path, PathBuf};
use thiserror::Error;
use uuid::Uuid;

pub const SESSION_VERSION: u32 = 1;
const SESSION_FILE: &str = "session.json";

#[derive(Debug, Serialize, Deserialize)]
struct SessionFile {
    version: u32,
    #[serde(default = "default_sidebar")]
    sidebar_visible: bool,
    #[serde(default)]
    active: Option<Uuid>,
    #[serde(default)]
    tabs: Vec<Uuid>,
}

fn default_sidebar() -> bool {
    true
}

#[derive(Debug, Clone, Serialize)]
pub struct SessionView {
    pub sidebar_visible: bool,
    pub active: Option<Uuid>,
    pub tabs: Vec<Uuid>,
}

#[derive(Debug, Error)]
pub enum SessionError {
    #[error("write {path}: {source}")]
    Write {
        path: PathBuf,
        source: std::io::Error,
    },
    #[error("encode {path}: {source}")]
    Encode {
        path: PathBuf,
        source: serde_json::Error,
    },
}

pub struct SessionStore {
    path: PathBuf,
    data: SessionFile,
    needs_save: bool,
}

impl SessionStore {
    /// A corrupt or unknown-version file starts an empty session and is marked
    /// for rewriting; it never prevents the app from starting.
    pub fn load(dir: &Path) -> Self {
        let path = dir.join(SESSION_FILE);
        let mut needs_save = false;
        let data = if path.exists() {
            match fs::read_to_string(&path) {
                Ok(raw) => match serde_json::from_str::<SessionFile>(&raw) {
                    Ok(file) if file.version == SESSION_VERSION => file,
                    Ok(file) => {
                        eprintln!(
                            "session: unsupported version {}; starting empty",
                            file.version
                        );
                        needs_save = true;
                        empty()
                    }
                    Err(error) => {
                        eprintln!("session: {error}; starting empty");
                        needs_save = true;
                        empty()
                    }
                },
                Err(error) => {
                    eprintln!("session: {error}; starting empty");
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

    pub fn sidebar_visible(&self) -> bool {
        self.data.sidebar_visible
    }

    pub fn active(&self) -> Option<Uuid> {
        self.data.active
    }

    pub fn tabs(&self) -> &[Uuid] {
        &self.data.tabs
    }

    pub fn needs_save(&self) -> bool {
        self.needs_save
    }

    /// Drop ids that no longer belong to an instance. Returns true when the
    /// session changed, so the caller can persist the repaired file.
    pub fn prune(&mut self, valid: &HashSet<Uuid>) -> bool {
        let before_tabs = self.data.tabs.clone();
        let before_active = self.data.active;
        self.data.tabs.retain(|id| valid.contains(id));
        if let Some(active) = self.data.active {
            if !self.data.tabs.contains(&active) {
                self.data.active = self.data.tabs.first().copied();
            }
        }
        if self.data.active.is_none() {
            self.data.active = self.data.tabs.first().copied();
        }
        before_tabs != self.data.tabs || before_active != self.data.active
    }

    pub fn view(&self) -> SessionView {
        SessionView {
            sidebar_visible: self.data.sidebar_visible,
            active: self.data.active,
            tabs: self.data.tabs.clone(),
        }
    }

    pub fn save(
        &mut self,
        sidebar_visible: bool,
        tabs: Vec<Uuid>,
        active: Option<Uuid>,
    ) -> Result<(), SessionError> {
        let active = active
            .filter(|id| tabs.contains(id))
            .or_else(|| tabs.first().copied());
        let file = SessionFile {
            version: SESSION_VERSION,
            sidebar_visible,
            active,
            tabs,
        };
        let bytes = serde_json::to_vec_pretty(&file).map_err(|source| SessionError::Encode {
            path: self.path.clone(),
            source,
        })?;

        if let Some(parent) = self.path.parent() {
            fs::create_dir_all(parent).map_err(|source| SessionError::Write {
                path: parent.to_path_buf(),
                source,
            })?;
        }
        let tmp = self.path.with_extension("json.tmp");
        {
            let mut out = OpenOptions::new()
                .write(true)
                .create(true)
                .truncate(true)
                .mode(0o644)
                .open(&tmp)
                .map_err(|source| SessionError::Write {
                    path: tmp.clone(),
                    source,
                })?;
            out.write_all(&bytes)
                .map_err(|source| SessionError::Write {
                    path: tmp.clone(),
                    source,
                })?;
            out.sync_all().map_err(|source| SessionError::Write {
                path: tmp.clone(),
                source,
            })?;
        }
        fs::rename(&tmp, &self.path).map_err(|source| SessionError::Write {
            path: self.path.clone(),
            source,
        })?;
        self.data = file;
        self.needs_save = false;
        Ok(())
    }
}

fn empty() -> SessionFile {
    SessionFile {
        version: SESSION_VERSION,
        sidebar_visible: true,
        active: None,
        tabs: Vec::new(),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::os::unix::fs::PermissionsExt;

    #[test]
    fn round_trips_and_uses_0644() {
        let dir = tempfile::tempdir().expect("tempdir");
        let first = Uuid::new_v4();
        let second = Uuid::new_v4();
        let mut session = SessionStore::load(dir.path());
        session
            .save(false, vec![first, second], Some(second))
            .expect("save");

        let path = dir.path().join(SESSION_FILE);
        let mode = fs::metadata(&path).expect("metadata").permissions().mode() & 0o777;
        assert_eq!(mode, 0o644);

        let reloaded = SessionStore::load(dir.path());
        assert!(!reloaded.sidebar_visible());
        assert_eq!(reloaded.tabs(), &[first, second]);
        assert_eq!(reloaded.active(), Some(second));
    }

    #[test]
    fn an_active_tab_outside_the_list_falls_back_to_the_first() {
        let dir = tempfile::tempdir().expect("tempdir");
        let first = Uuid::new_v4();
        let other = Uuid::new_v4();
        let mut session = SessionStore::load(dir.path());
        session.save(true, vec![first], Some(other)).expect("save");
        assert_eq!(session.active(), Some(first));
    }

    #[test]
    fn corrupt_and_unknown_versions_start_empty_and_ask_to_save() {
        for content in ["{", r#"{"version":99,"tabs":[]}"#] {
            let dir = tempfile::tempdir().expect("tempdir");
            fs::write(dir.path().join(SESSION_FILE), content).expect("write");
            let session = SessionStore::load(dir.path());
            assert!(session.tabs().is_empty());
            assert!(session.sidebar_visible());
            assert!(session.needs_save());
        }
    }

    #[test]
    fn prune_drops_unknown_instances_and_repairs_the_active_tab() {
        let dir = tempfile::tempdir().expect("tempdir");
        let kept = Uuid::new_v4();
        let removed = Uuid::new_v4();
        let mut session = SessionStore::load(dir.path());
        session
            .save(true, vec![removed, kept], Some(removed))
            .expect("save");

        let valid = HashSet::from([kept]);
        assert!(session.prune(&valid));
        assert_eq!(session.tabs(), &[kept]);
        assert_eq!(session.active(), Some(kept));
        assert!(!session.prune(&valid));
    }
}
