//! macOS Keychain storage for a Hub API token.
//!
//! The registry (`instances.json`) must never hold a secret, so the token is kept
//! in the login keychain under a service name and the instance id as the account.
//! `security` is the only system interface used here: the desktop is macOS-only,
//! and adding a keychain crate for two commands would not make it safer.

use std::process::Command;
use uuid::Uuid;

const SERVICE: &str = "cn.yusiwen.flowhub.hub-token";

fn account(id: Uuid) -> String {
    id.to_string()
}

#[cfg(target_os = "macos")]
pub fn load(id: Uuid) -> Option<String> {
    let output = Command::new("security")
        .args([
            "find-generic-password",
            "-s",
            SERVICE,
            "-a",
            &account(id),
            "-w",
        ])
        .output()
        .ok()?;
    if !output.status.success() {
        return None;
    }
    let token = String::from_utf8_lossy(&output.stdout).trim().to_string();
    if token.is_empty() {
        None
    } else {
        Some(token)
    }
}

#[cfg(not(target_os = "macos"))]
pub fn load(_id: Uuid) -> Option<String> {
    None
}

#[cfg(target_os = "macos")]
pub fn save(id: Uuid, token: &str) -> Result<(), String> {
    let output = Command::new("security")
        .args([
            "add-generic-password",
            "-U",
            "-s",
            SERVICE,
            "-a",
            &account(id),
            "-w",
            token,
        ])
        .output()
        .map_err(|error| format!("run security: {error}"))?;
    if output.status.success() {
        Ok(())
    } else {
        let detail = String::from_utf8_lossy(&output.stderr).trim().to_string();
        Err(if detail.is_empty() {
            "the macOS Keychain refused the token".to_string()
        } else {
            format!("the macOS Keychain refused the token: {detail}")
        })
    }
}

#[cfg(not(target_os = "macos"))]
pub fn save(_id: Uuid, _token: &str) -> Result<(), String> {
    Err("the macOS Keychain is only available in the macOS app".to_string())
}

#[cfg(target_os = "macos")]
pub fn remove(id: Uuid) {
    let _ = Command::new("security")
        .args(["delete-generic-password", "-s", SERVICE, "-a", &account(id)])
        .output();
}

#[cfg(not(target_os = "macos"))]
pub fn remove(_id: Uuid) {}
