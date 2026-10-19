//! The on-disk instance registry: one versioned JSON file, written atomically
//! with a `0600` mode. The file holds no secret; dsh launch tokens are transient
//! and must never be persisted here.

use crate::model::{Instance, InstanceInput};
use serde::{Deserialize, Serialize};
use std::fs::{self, OpenOptions};
use std::io::Write;
use std::os::unix::fs::OpenOptionsExt;
use std::path::{Path, PathBuf};
use std::sync::MutexGuard;
use thiserror::Error;
use uuid::Uuid;

pub const REGISTRY_VERSION: u32 = 1;
const REGISTRY_FILE: &str = "instances.json";

#[derive(Debug, Serialize, Deserialize)]
struct RegistryFile {
    version: u32,
    instances: Vec<Instance>,
}

#[derive(Debug, Error)]
pub enum StoreError {
    #[error("read {path}: {source}")]
    Read {
        path: PathBuf,
        source: std::io::Error,
    },
    #[error("parse {path}: {source}")]
    Parse {
        path: PathBuf,
        source: serde_json::Error,
    },
    #[error("encode {path}: {source}")]
    Encode {
        path: PathBuf,
        source: serde_json::Error,
    },
    #[error("unsupported registry version {version} in {path}")]
    Version { version: u32, path: PathBuf },
    #[error("invalid instance #{index} in {path}: {reason}")]
    Invalid {
        index: usize,
        path: PathBuf,
        reason: String,
    },
    #[error("write {path}: {source}")]
    Write {
        path: PathBuf,
        source: std::io::Error,
    },
    #[error("instance not found: {0}")]
    NotFound(Uuid),
    #[error("invalid input: {0}")]
    Input(String),
}

#[derive(Debug)]
pub struct Store {
    path: PathBuf,
    backup: PathBuf,
    instances: Vec<Instance>,
}

impl Store {
    pub fn load(dir: &Path) -> Result<Self, StoreError> {
        let path = dir.join(REGISTRY_FILE);
        let backup = dir.join(format!("{REGISTRY_FILE}.bak"));
        let mut instances = if path.exists() {
            let raw = fs::read_to_string(&path).map_err(|source| StoreError::Read {
                path: path.clone(),
                source,
            })?;
            let file: RegistryFile =
                serde_json::from_str(&raw).map_err(|source| StoreError::Parse {
                    path: path.clone(),
                    source,
                })?;
            if file.version != REGISTRY_VERSION {
                return Err(StoreError::Version {
                    version: file.version,
                    path,
                });
            }
            let mut seen = std::collections::HashSet::new();
            for (index, instance) in file.instances.iter().enumerate() {
                instance
                    .validate_loaded()
                    .map_err(|reason| StoreError::Invalid {
                        index,
                        path: path.clone(),
                        reason,
                    })?;
                if !seen.insert(instance.id) {
                    return Err(StoreError::Invalid {
                        index,
                        path: path.clone(),
                        reason: "duplicate instance id".to_string(),
                    });
                }
            }
            file.instances
        } else {
            Vec::new()
        };
        instances.sort_by_key(|instance| instance.order);
        Ok(Self {
            path,
            backup,
            instances,
        })
    }

    pub fn instances(&self) -> &[Instance] {
        &self.instances
    }

    pub fn add(&mut self, input: &InstanceInput) -> Result<Instance, StoreError> {
        let order = self
            .instances
            .iter()
            .map(|instance| instance.order)
            .max()
            .unwrap_or(-1)
            + 1;
        let instance = Instance::from_input(input, order).map_err(StoreError::Input)?;
        self.instances.push(instance.clone());
        self.save()?;
        Ok(instance)
    }

    pub fn update(&mut self, id: Uuid, input: &InstanceInput) -> Result<Instance, StoreError> {
        let instance = self
            .instances
            .iter_mut()
            .find(|instance| instance.id == id)
            .ok_or(StoreError::NotFound(id))?;
        instance.apply(input).map_err(StoreError::Input)?;
        let updated = instance.clone();
        self.save()?;
        Ok(updated)
    }

    pub fn remove(&mut self, id: Uuid) -> Result<(), StoreError> {
        let before = self.instances.len();
        self.instances.retain(|instance| instance.id != id);
        if self.instances.len() == before {
            return Err(StoreError::NotFound(id));
        }
        self.renumber();
        self.save()
    }

    pub fn reorder(&mut self, ids: &[Uuid]) -> Result<(), StoreError> {
        if ids.len() != self.instances.len() {
            return Err(StoreError::Input(
                "reorder must list every instance exactly once".to_string(),
            ));
        }
        let mut seen = std::collections::HashSet::new();
        for id in ids {
            if !self.instances.iter().any(|instance| instance.id == *id) {
                return Err(StoreError::Input(format!("unknown instance: {id}")));
            }
            if !seen.insert(*id) {
                return Err(StoreError::Input(format!("duplicate instance: {id}")));
            }
        }
        for (order, id) in ids.iter().enumerate() {
            if let Some(instance) = self
                .instances
                .iter_mut()
                .find(|instance| instance.id == *id)
            {
                instance.order = order as i64;
            }
        }
        self.instances.sort_by_key(|instance| instance.order);
        self.save()
    }

    fn renumber(&mut self) {
        self.instances.sort_by_key(|instance| instance.order);
        for (order, instance) in self.instances.iter_mut().enumerate() {
            instance.order = order as i64;
        }
    }

    fn save(&self) -> Result<(), StoreError> {
        if let Some(parent) = self.path.parent() {
            fs::create_dir_all(parent).map_err(|source| StoreError::Write {
                path: parent.to_path_buf(),
                source,
            })?;
        }

        let file = RegistryFile {
            version: REGISTRY_VERSION,
            instances: self.instances.clone(),
        };
        let bytes = serde_json::to_vec_pretty(&file).map_err(|source| StoreError::Encode {
            path: self.path.clone(),
            source,
        })?;

        // Keep one previous copy. A failed backup is not fatal: the new file is
        // still written atomically, and losing the backup is better than
        // refusing an edit.
        if self.path.exists() {
            let _ = fs::copy(&self.path, &self.backup);
        }

        let tmp = self.path.with_extension("json.tmp");
        {
            let mut out = OpenOptions::new()
                .write(true)
                .create(true)
                .truncate(true)
                .mode(0o600)
                .open(&tmp)
                .map_err(|source| StoreError::Write {
                    path: tmp.clone(),
                    source,
                })?;
            out.write_all(&bytes).map_err(|source| StoreError::Write {
                path: tmp.clone(),
                source,
            })?;
            out.sync_all().map_err(|source| StoreError::Write {
                path: tmp.clone(),
                source,
            })?;
        }
        fs::rename(&tmp, &self.path).map_err(|source| StoreError::Write {
            path: self.path.clone(),
            source,
        })
    }
}

/// The command layer locks the store for the duration of one mutation. Keeping
/// the helper here makes the lock error a single message instead of a panic.
pub fn lock<'a>(store: &'a std::sync::Mutex<Store>) -> Result<MutexGuard<'a, Store>, String> {
    store
        .lock()
        .map_err(|_| "instance registry lock poisoned".to_string())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::model::InstanceKind;
    use std::os::unix::fs::PermissionsExt;

    fn input(name: &str, kind: InstanceKind, url: &str) -> InstanceInput {
        InstanceInput {
            name: name.to_string(),
            kind,
            url: url.to_string(),
            group: None,
            notes: None,
        }
    }

    fn temp_store() -> (tempfile::TempDir, Store) {
        let dir = tempfile::tempdir().expect("tempdir");
        let store = Store::load(dir.path()).expect("load empty store");
        (dir, store)
    }

    #[test]
    fn round_trips_instances_and_uses_0600() {
        let (dir, mut store) = temp_store();
        let first = store
            .add(&input("dsh", InstanceKind::DshWeb, "http://127.0.0.1:3080"))
            .expect("add");
        let second = store
            .add(&input(
                "oc",
                InstanceKind::Opencode,
                "http://127.0.0.1:4096",
            ))
            .expect("add");
        assert_ne!(first.id, second.id);
        assert_eq!(first.order, 0);
        assert_eq!(second.order, 1);

        let path = dir.path().join(REGISTRY_FILE);
        let mode = fs::metadata(&path).expect("metadata").permissions().mode() & 0o777;
        assert_eq!(mode, 0o600);

        let reloaded = Store::load(dir.path()).expect("reload");
        assert_eq!(reloaded.instances(), store.instances());
    }

    #[test]
    fn unknown_version_is_refused() {
        let dir = tempfile::tempdir().expect("tempdir");
        fs::write(
            dir.path().join(REGISTRY_FILE),
            r#"{"version":99,"instances":[]}"#,
        )
        .expect("write");
        let error = Store::load(dir.path()).expect_err("version must be refused");
        assert!(
            error.to_string().contains("unsupported registry version"),
            "{error}"
        );
    }

    #[test]
    fn corrupt_json_is_refused() {
        let dir = tempfile::tempdir().expect("tempdir");
        fs::write(dir.path().join(REGISTRY_FILE), "{").expect("write");
        assert!(Store::load(dir.path()).is_err());
    }

    #[test]
    fn reorder_requires_every_id_once() {
        let (_dir, mut store) = temp_store();
        let first = store
            .add(&input("dsh", InstanceKind::DshWeb, "http://127.0.0.1:3080"))
            .expect("add");
        let second = store
            .add(&input(
                "oc",
                InstanceKind::Opencode,
                "http://127.0.0.1:4096",
            ))
            .expect("add");

        store.reorder(&[second.id, first.id]).expect("reorder both");
        assert_eq!(store.instances()[0].id, second.id);

        assert!(store.reorder(&[first.id]).is_err());
        assert!(store.reorder(&[first.id, first.id]).is_err());
    }

    #[test]
    fn remove_renumbers_and_persists() {
        let (dir, mut store) = temp_store();
        let first = store
            .add(&input("one", InstanceKind::Other, "http://127.0.0.1:1"))
            .expect("add");
        let second = store
            .add(&input("two", InstanceKind::Other, "http://127.0.0.1:2"))
            .expect("add");
        store.remove(first.id).expect("remove");
        assert_eq!(store.instances().len(), 1);
        assert_eq!(store.instances()[0].id, second.id);
        assert_eq!(store.instances()[0].order, 0);

        let reloaded = Store::load(dir.path()).expect("reload");
        assert_eq!(reloaded.instances()[0].id, second.id);
    }
}
