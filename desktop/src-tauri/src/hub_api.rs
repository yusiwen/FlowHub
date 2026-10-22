//! Read-only client for the service's `/hub/v1` API.
//!
//! The shell cannot fetch a URL itself (its CSP allows only Tauri IPC), so the
//! request and the bearer token stay in Rust. A login token is never written to
//! the instance registry; a loopback Hub may supply it from the operator's
//! 0600 `.env`, otherwise the shell asks for it and keeps it in memory.

use crate::hub;
use crate::model::healthz_url;
use serde_json::Value;
use std::error::Error as _;
use std::fs;
use std::path::{Path, PathBuf};
use std::time::Duration;
use url::Url;

const MAX_BODY_BYTES: usize = 2 * 1024 * 1024;
const TIMEOUT: Duration = Duration::from_secs(4);

/// Join a base address to `/hub/v1/<path>`, preserving a reverse-proxy prefix.
pub fn api_url(base: &str, path: &str) -> Result<Url, String> {
    if path.is_empty() || path.starts_with('/') {
        return Err("the hub API path must be relative".to_string());
    }
    let mut url = healthz_url(base)?;
    let prefix = url
        .path()
        .strip_suffix("healthz")
        .unwrap_or("/")
        .to_string();
    url.set_path(&format!("{prefix}hub/v1/{path}"));
    url.set_query(None);
    Ok(url)
}

/// Parse `FLOWHUB_HUB_TOKEN` from an env file. The parser is deliberately small:
/// comments, blank lines, `export `, one `=`, optional matching quotes.
pub fn parse_env_token(text: &str) -> Option<String> {
    for line in text.lines() {
        let line = line.trim();
        if line.is_empty() || line.starts_with('#') {
            continue;
        }
        let line = line.strip_prefix("export ").unwrap_or(line);
        let Some((key, value)) = line.split_once('=') else {
            continue;
        };
        if key.trim() != "FLOWHUB_HUB_TOKEN" {
            continue;
        }
        let value = value.trim();
        let value = if value.len() >= 2
            && ((value.starts_with('"') && value.ends_with('"'))
                || (value.starts_with('\'') && value.ends_with('\'')))
        {
            &value[1..value.len() - 1]
        } else {
            value
        };
        if !value.is_empty() {
            return Some(value.to_string());
        }
    }
    None
}

fn candidate_env_files(data_dir: Option<&str>) -> Vec<PathBuf> {
    let mut files = Vec::new();
    if let Ok(file) = std::env::var("FLOWHUB_ENV_FILE") {
        let file = file.trim();
        if !file.is_empty() && file != "-" && file != "none" && file != "off" {
            files.push(PathBuf::from(file));
        }
    }
    if let Some(data_dir) = data_dir.map(str::trim).filter(|dir| !dir.is_empty()) {
        files.push(Path::new(data_dir).join(".env"));
    }
    if let Ok(config) = std::env::var("XDG_CONFIG_HOME") {
        files.push(Path::new(&config).join("flowhub").join(".env"));
    }
    if let Ok(home) = std::env::var("HOME") {
        files.push(
            Path::new(&home)
                .join(".config")
                .join("flowhub")
                .join(".env"),
        );
    }
    files
}

/// Read the token from the operator's env file. Callers must first establish that
/// the Hub is on this machine; a remote service must never make the app read a
/// local file.
pub fn local_token(data_dir: Option<&str>) -> Option<String> {
    for file in candidate_env_files(data_dir) {
        let Ok(text) = fs::read_to_string(&file) else {
            continue;
        };
        if let Some(token) = parse_env_token(&text) {
            return Some(token);
        }
    }
    None
}

/// Resolve the token to send. A provided token wins; a loopback Hub falls back to
/// the local env file; anything else is refused before a request is attempted.
pub fn resolve_token(
    base: &str,
    provided: Option<&str>,
    data_dir: Option<&str>,
) -> Result<String, String> {
    if let Some(token) = provided.map(str::trim).filter(|token| !token.is_empty()) {
        return Ok(token.to_string());
    }
    if hub::is_loopback(base) {
        if let Some(token) = local_token(data_dir) {
            return Ok(token);
        }
        return Err("the hub API needs FLOWHUB_HUB_TOKEN; set it in ~/.config/flowhub/.env or paste it here".to_string());
    }
    Err("the hub API needs a token for a remote FlowHub; paste it here".to_string())
}

fn reason(error: &reqwest::Error) -> String {
    let mut text = String::new();
    let mut cause = error.source();
    while let Some(error) = cause {
        let part = error.to_string();
        if !part.is_empty() {
            if !text.is_empty() {
                text.push_str(": ");
            }
            text.push_str(&part);
        }
        cause = error.source();
    }
    if text.is_empty() {
        error.to_string()
    } else {
        text
    }
}

pub async fn get(base: &str, path: &str, token: &str) -> Result<Value, String> {
    let url = api_url(base, path)?;
    let client = reqwest::Client::builder()
        .timeout(TIMEOUT)
        .redirect(reqwest::redirect::Policy::none())
        .build()
        .map_err(|error| format!("build the HTTP client: {error}"))?;
    let mut response = client
        .get(url.clone())
        .bearer_auth(token)
        .send()
        .await
        .map_err(|error| format!("{url} is unreachable: {}", reason(&error)))?;
    let status = response.status();
    if !status.is_success() {
        return Err(match status.as_u16() {
            401 | 403 => format!("{url} rejected the hub token (HTTP {})", status.as_u16()),
            503 => format!(
                "{url} has no hub API enabled (HTTP 503); set FLOWHUB_HUB_TOKEN on the service"
            ),
            _ => format!("{url} answered HTTP {}", status.as_u16()),
        });
    }
    let mut body = Vec::new();
    while let Some(chunk) = response
        .chunk()
        .await
        .map_err(|error| format!("read {url}: {error}"))?
    {
        if body.len() + chunk.len() > MAX_BODY_BYTES {
            return Err(format!(
                "{url} answered with more than {MAX_BODY_BYTES} bytes"
            ));
        }
        body.extend_from_slice(&chunk);
    }
    serde_json::from_slice(&body).map_err(|error| format!("{url}: invalid JSON: {error}"))
}

/// A flow id and a run id become path segments; refuse anything that could escape
/// the collection instead of trusting the service's echo.
pub fn segment(value: &str) -> Result<&str, String> {
    if value.is_empty()
        || value == "."
        || value == ".."
        || !value
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'-' | b'_' | b'.'))
    {
        return Err("invalid hub resource id".to_string());
    }
    Ok(value)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn env_file(text: &str) -> Option<String> {
        parse_env_token(text)
    }

    #[test]
    fn the_hub_api_url_keeps_a_reverse_proxy_prefix() {
        assert_eq!(
            api_url("http://127.0.0.1:8080", "flows")
                .expect("base")
                .as_str(),
            "http://127.0.0.1:8080/hub/v1/flows"
        );
        assert_eq!(
            api_url("http://host/flowhub", "flows/default-youtrack")
                .expect("prefix")
                .as_str(),
            "http://host/flowhub/hub/v1/flows/default-youtrack"
        );
        assert_eq!(
            api_url("http://host/flowhub/", "runs")
                .expect("slash")
                .as_str(),
            "http://host/flowhub/hub/v1/runs"
        );
        assert!(api_url("ftp://host", "flows").is_err());
        assert!(api_url("http://host", "/flows").is_err());
    }

    #[test]
    fn the_env_parser_reads_only_the_hub_token() {
        assert_eq!(
            env_file("# comment\nFLOWHUB_HOOK_KEY=abc\nFLOWHUB_HUB_TOKEN=secret\n").as_deref(),
            Some("secret")
        );
        assert_eq!(
            env_file("export FLOWHUB_HUB_TOKEN=\"quoted\"\n").as_deref(),
            Some("quoted")
        );
        assert_eq!(
            env_file("FLOWHUB_HUB_TOKEN='single'\n").as_deref(),
            Some("single")
        );
        assert_eq!(env_file("FLOWHUB_HUB_TOKEN=\n"), None);
        assert_eq!(env_file("OTHER_TOKEN=secret\n"), None);
    }

    #[test]
    fn a_provided_token_wins_and_a_remote_hub_needs_one() {
        assert_eq!(
            resolve_token("http://127.0.0.1:8080", Some("given"), None).expect("provided"),
            "given"
        );
        assert!(resolve_token("https://remote.example", None, None).is_err());
    }

    #[test]
    fn resource_ids_cannot_escape_the_collection() {
        assert_eq!(
            segment("default-youtrack").expect("flow"),
            "default-youtrack"
        );
        assert_eq!(segment("run_123").expect("run"), "run_123");
        assert!(segment("").is_err());
        assert!(segment(".").is_err());
        assert!(segment("..").is_err());
        assert!(segment("../admin").is_err());
        assert!(segment("a/b").is_err());
    }
}
