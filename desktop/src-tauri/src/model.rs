//! The instance registry model. Validation lives here so the store and the
//! IPC commands cannot disagree about what a valid instance is.

use serde::{Deserialize, Serialize};
use url::Url;
use uuid::Uuid;

const MAX_NAME_CHARS: usize = 120;
const MAX_NOTES_CHARS: usize = 2000;

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "kebab-case")]
pub enum InstanceKind {
    DshWeb,
    Opencode,
    Other,
}

impl InstanceKind {
    /// A dsh web instance always authenticates with the launch token. The other
    /// kinds are loaded directly; a future Basic Auth mode can be added here.
    pub fn default_auth(self) -> AuthMode {
        match self {
            Self::DshWeb => AuthMode::DshToken,
            Self::Opencode | Self::Other => AuthMode::None,
        }
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "kebab-case")]
pub enum AuthMode {
    None,
    DshToken,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct Instance {
    pub id: Uuid,
    pub name: String,
    pub kind: InstanceKind,
    pub url: String,
    pub auth: AuthMode,
    pub group: Option<String>,
    pub order: i64,
    pub notes: String,
}

/// The fields the shell may submit. `id`, `order` and `auth` are derived by the
/// backend: a client cannot pick an instance id or forge an auth mode.
#[derive(Debug, Clone, Deserialize)]
pub struct InstanceInput {
    pub name: String,
    pub kind: InstanceKind,
    pub url: String,
    #[serde(default)]
    pub group: Option<String>,
    #[serde(default)]
    pub notes: Option<String>,
}

#[derive(Debug)]
pub struct ValidatedInstance {
    pub name: String,
    pub kind: InstanceKind,
    pub url: String,
    pub group: Option<String>,
    pub notes: String,
}

impl InstanceInput {
    pub fn validate(&self) -> Result<ValidatedInstance, String> {
        let name = self.name.trim();
        if name.is_empty() {
            return Err("name is required".to_string());
        }
        if name.chars().count() > MAX_NAME_CHARS {
            return Err(format!("name must be at most {MAX_NAME_CHARS} characters"));
        }

        let url = normalize_url(&self.url)?;
        let group = self
            .group
            .as_deref()
            .map(str::trim)
            .filter(|group| !group.is_empty())
            .map(ToOwned::to_owned);
        let notes = self.notes.as_deref().unwrap_or("").trim().to_string();
        if notes.chars().count() > MAX_NOTES_CHARS {
            return Err(format!(
                "notes must be at most {MAX_NOTES_CHARS} characters"
            ));
        }

        Ok(ValidatedInstance {
            name: name.to_string(),
            kind: self.kind,
            url,
            group,
            notes,
        })
    }
}

impl Instance {
    pub fn from_input(input: &InstanceInput, order: i64) -> Result<Self, String> {
        let valid = input.validate()?;
        Ok(Self {
            id: Uuid::new_v4(),
            name: valid.name,
            auth: valid.kind.default_auth(),
            kind: valid.kind,
            url: valid.url,
            group: valid.group,
            order,
            notes: valid.notes,
        })
    }

    pub fn apply(&mut self, input: &InstanceInput) -> Result<(), String> {
        let valid = input.validate()?;
        self.name = valid.name;
        self.kind = valid.kind;
        self.auth = valid.kind.default_auth();
        self.url = valid.url;
        self.group = valid.group;
        self.notes = valid.notes;
        Ok(())
    }

    /// Re-check a row loaded from disk. A hand-edited registry must not reach a
    /// webview with an unsupported scheme.
    pub fn validate_loaded(&self) -> Result<(), String> {
        if self.name.trim().is_empty() {
            return Err("name is empty".to_string());
        }
        normalize_url(&self.url)?;
        Ok(())
    }
}

fn normalize_url(raw: &str) -> Result<String, String> {
    let raw = raw.trim();
    if raw.is_empty() {
        return Err("url is required".to_string());
    }
    let url = Url::parse(raw).map_err(|error| format!("url is not valid: {error}"))?;
    if !matches!(url.scheme(), "http" | "https") {
        return Err("url must use http or https".to_string());
    }
    if url.host_str().is_none() {
        return Err("url must name a host".to_string());
    }
    if !url.username().is_empty() || url.password().is_some() {
        return Err("url must not embed a username or password".to_string());
    }
    Ok(url.to_string())
}

#[cfg(test)]
mod tests {
    use super::*;

    fn input(name: &str, kind: InstanceKind, url: &str) -> InstanceInput {
        InstanceInput {
            name: name.to_string(),
            kind,
            url: url.to_string(),
            group: None,
            notes: None,
        }
    }

    #[test]
    fn http_and_https_are_accepted_and_normalized() {
        let valid = input("dsh", InstanceKind::DshWeb, "http://127.0.0.1:3080")
            .validate()
            .expect("valid");
        assert_eq!(valid.url, "http://127.0.0.1:3080/");

        let valid = input("oc", InstanceKind::Opencode, "https://nuc11.example:4096/")
            .validate()
            .expect("valid");
        assert_eq!(valid.url, "https://nuc11.example:4096/");
    }

    #[test]
    fn other_schemes_are_rejected() {
        let error = input("bad", InstanceKind::Other, "ftp://example.com")
            .validate()
            .expect_err("ftp must be rejected");
        assert!(error.contains("http or https"), "{error}");
    }

    #[test]
    fn embedded_credentials_are_rejected() {
        let error = input(
            "bad",
            InstanceKind::Opencode,
            "https://user:pass@example.com",
        )
        .validate()
        .expect_err("userinfo must be rejected");
        assert!(error.contains("username or password"), "{error}");
    }

    #[test]
    fn empty_and_overlong_names_are_rejected() {
        assert!(input("   ", InstanceKind::Other, "http://127.0.0.1:1")
            .validate()
            .is_err());
        let long = "x".repeat(MAX_NAME_CHARS + 1);
        assert!(input(&long, InstanceKind::Other, "http://127.0.0.1:1")
            .validate()
            .is_err());
    }

    #[test]
    fn empty_group_becomes_none() {
        let mut candidate = input("dsh", InstanceKind::DshWeb, "http://127.0.0.1:3080");
        candidate.group = Some("   ".to_string());
        assert_eq!(candidate.validate().expect("valid").group, None);
    }

    #[test]
    fn auth_is_derived_from_kind() {
        let dsh = Instance::from_input(
            &input("dsh", InstanceKind::DshWeb, "http://127.0.0.1:3080"),
            0,
        )
        .expect("valid");
        assert_eq!(dsh.auth, AuthMode::DshToken);

        let oc = Instance::from_input(
            &input("oc", InstanceKind::Opencode, "http://127.0.0.1:4096"),
            0,
        )
        .expect("valid");
        assert_eq!(oc.auth, AuthMode::None);
    }
}
