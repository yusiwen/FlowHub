//! Reachability probes for the sidebar. A dsh 401 means "reachable, sign-in
//! required", not "down"; the probe therefore has its own status class instead
//! of treating every non-2xx response as a failure.
//!
//! dsh answers `401` to every request that does not carry its
//! `dsh-auth-<authority hash>` cookie, so a credential-less probe cannot tell a
//! signed-in instance from a signed-out one. The probe therefore reuses the
//! cookie the tab's webview already holds: the value is read at probe time, kept
//! for that one request, and never logged, persisted, or sent to the shell.

use crate::auth;
use crate::model::{AuthMode, Instance};
use crate::state::{lock_tabs, AppState};
use crate::store::lock;
use serde::{Deserialize, Serialize};
use std::collections::HashMap;
use std::sync::atomic::Ordering;
use std::time::{Duration, Instant};
use tauri::{AppHandle, Emitter, Manager, Webview};
use url::Url;
use uuid::Uuid;

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "kebab-case")]
pub enum HealthStatus {
    Ok,
    Auth,
    Http,
    Unreachable,
}

/// What the probe carried. A dsh `401` with `None` means the app has no session
/// for that instance yet; with `Cookie` it means the server rejected a session
/// the tab still holds.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "kebab-case")]
pub enum Credential {
    None,
    Cookie,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct HealthReport {
    pub instance_id: Uuid,
    pub status: HealthStatus,
    pub latency_ms: u64,
    pub credential: Credential,
}

pub fn classify(code: u16) -> HealthStatus {
    match code {
        200..=399 => HealthStatus::Ok,
        401 | 403 => HealthStatus::Auth,
        _ => HealthStatus::Http,
    }
}

/// A probe that carried the tab's own cookie and came back `Auth` has proved the
/// session dead: the shell must ask for a token again instead of keeping a page
/// that only answers 401. A rejection without a credential says nothing about the
/// session and never triggers a prompt.
pub fn session_rejected(auth: AuthMode, report: &HealthReport) -> bool {
    auth == AuthMode::DshToken
        && report.status == HealthStatus::Auth
        && report.credential == Credential::Cookie
}

pub async fn probe_all(app: &AppHandle) -> Vec<HealthReport> {
    if app.state::<AppState>().probing.swap(true, Ordering::SeqCst) {
        return Vec::new();
    }

    let (instances, cookies) = {
        let state = app.state::<AppState>();
        let instances = match lock(&state.store) {
            Ok(guard) => guard.instances().to_vec(),
            Err(_) => {
                state.probing.store(false, Ordering::SeqCst);
                return Vec::new();
            }
        };
        let cookies = dsh_cookies(app, &instances);
        (instances, cookies)
    };

    let client = match reqwest::Client::builder()
        .timeout(Duration::from_secs(3))
        .redirect(reqwest::redirect::Policy::none())
        .build()
    {
        Ok(client) => client,
        Err(_) => {
            app.state::<AppState>()
                .probing
                .store(false, Ordering::SeqCst);
            return Vec::new();
        }
    };

    let mut reports = Vec::with_capacity(instances.len());
    for instance in instances {
        let probe_cookie = cookies.get(&instance.id).cloned();
        let report = probe_one(&client, &instance, probe_cookie.as_deref()).await;
        {
            let state = app.state::<AppState>();
            let guard = state.health.lock();
            if let Ok(mut health) = guard {
                health.insert(report.instance_id, report.clone());
            }
        }
        let _ = app.emit("health://changed", report.clone());
        if session_rejected(instance.auth, &report) {
            // The tab may have been signed in again while this probe was in
            // flight, so only a verdict about the cookie the tab holds *now* may
            // put it back into sign-in. The health lock is released above; the
            // tab manager is taken after it and never the other way round.
            let current = dsh_cookies(app, std::slice::from_ref(&instance));
            if current.get(&instance.id) == probe_cookie.as_ref() {
                let state = app.state::<AppState>();
                let tabs = lock_tabs(&state);
                if let Ok(mut tabs) = tabs {
                    tabs.request_sign_in(app, instance.id);
                }
            }
        }
        reports.push(report);
    }

    app.state::<AppState>()
        .probing
        .store(false, Ordering::SeqCst);
    reports
}

/// The `Cookie` header for each loaded dsh tab, by instance id.
fn dsh_cookies(app: &AppHandle, instances: &[Instance]) -> HashMap<Uuid, String> {
    // Take the webview handles under the lock and read cookies without it:
    // `Webview::cookies()` blocks on the main thread, and the main thread can be
    // waiting for the tab-manager mutex this function would still be holding.
    let loaded = {
        let state = app.state::<AppState>();
        let tabs = match lock_tabs(&state) {
            Ok(tabs) => tabs,
            Err(_) => return HashMap::new(),
        };
        tabs.loaded_webviews()
    };

    let mut cookies = HashMap::new();
    for (id, webview, base) in loaded {
        let is_dsh = instances
            .iter()
            .any(|instance| instance.id == id && instance.auth == AuthMode::DshToken);
        if !is_dsh {
            continue;
        }
        if let Some(header) = cookie_header(&webview, &base) {
            cookies.insert(id, header);
        }
    }
    cookies
}

/// The `name=value` pair of the dsh session cookie, when the webview holds one.
fn cookie_header(webview: &Webview, base: &Url) -> Option<String> {
    let expected = auth::cookie_name(base);
    let cookies = webview.cookies().ok()?;
    let cookie = cookies
        .iter()
        .find(|cookie| cookie.name() == expected.as_str())?;
    Some(format!("{}={}", cookie.name(), cookie.value()))
}

/// Verify a freshly loaded tab with the cookie it holds, and put it back into
/// sign-in when the server rejects that cookie. Runs on the async runtime because
/// reading cookies blocks on the main thread, and the caller may hold the tab
/// manager's lock.
pub fn verify_tab(app: AppHandle, instance: Instance) {
    if instance.auth != AuthMode::DshToken {
        return;
    }
    tauri::async_runtime::spawn(async move {
        let Some(cookie) = tab_cookie(&app, &instance) else {
            return;
        };
        let Ok(client) = reqwest::Client::builder()
            .timeout(Duration::from_secs(3))
            .redirect(reqwest::redirect::Policy::none())
            .build()
        else {
            return;
        };
        let report = probe_one(&client, &instance, Some(&cookie)).await;
        if session_rejected(instance.auth, &report) {
            let state = app.state::<AppState>();
            let tabs = lock_tabs(&state);
            if let Ok(mut tabs) = tabs {
                tabs.request_sign_in(&app, instance.id);
            }
        }
    });
}

/// The `Cookie` header a loaded tab holds for this instance.
fn tab_cookie(app: &AppHandle, instance: &Instance) -> Option<String> {
    let base = Url::parse(&instance.url).ok()?;
    let webview = {
        let state = app.state::<AppState>();
        let tabs = lock_tabs(&state).ok()?;
        tabs.loaded_webviews()
            .into_iter()
            .find(|(id, _, _)| *id == instance.id)
            .map(|(_, webview, _)| webview)
    };
    cookie_header(&webview?, &base)
}

async fn probe_one(
    client: &reqwest::Client,
    instance: &Instance,
    cookie: Option<&str>,
) -> HealthReport {
    let credential = if cookie.is_some() {
        Credential::Cookie
    } else {
        Credential::None
    };
    let started = Instant::now();
    // A hub has no page of its own; /healthz is what says whether it is alive.
    let target = if instance.is_dashboard() {
        crate::model::healthz_url(&instance.url)
            .ok()
            .map(|url| url.to_string())
    } else {
        Some(instance.url.clone())
    };
    let status = match target {
        None => HealthStatus::Unreachable,
        Some(target) => {
            let mut request = client.get(target);
            if let Some(cookie) = cookie {
                request = request.header(reqwest::header::COOKIE, cookie);
            }
            match request.send().await {
                Ok(response) => classify(response.status().as_u16()),
                Err(_) => HealthStatus::Unreachable,
            }
        }
    };
    HealthReport {
        instance_id: instance.id,
        status,
        latency_ms: started.elapsed().as_millis().min(u128::from(u64::MAX)) as u64,
        credential,
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::model::InstanceKind;
    use std::io::{Read, Write};
    use std::net::TcpListener;
    use std::thread::JoinHandle;

    fn client() -> reqwest::Client {
        reqwest::Client::builder()
            .timeout(Duration::from_secs(3))
            .redirect(reqwest::redirect::Policy::none())
            .build()
            .expect("client")
    }

    fn dsh(url: String) -> Instance {
        Instance {
            id: Uuid::new_v4(),
            name: "dsh".to_string(),
            kind: InstanceKind::DshWeb,
            url,
            auth: AuthMode::DshToken,
            group: None,
            order: 0,
            notes: String::new(),
        }
    }

    /// Serve exactly one request and return the head it received, lowercased.
    fn serve(status: u16) -> (String, JoinHandle<String>) {
        let listener = TcpListener::bind("127.0.0.1:0").expect("bind");
        let url = format!("http://{}/", listener.local_addr().expect("addr"));
        let handle = std::thread::spawn(move || {
            let (mut stream, _) = listener.accept().expect("accept");
            let mut buffer = [0u8; 8192];
            let read = stream.read(&mut buffer).expect("read");
            let head = String::from_utf8_lossy(&buffer[..read]).to_lowercase();
            let _ = stream.write_all(
                format!("HTTP/1.1 {status} X\r\ncontent-length: 0\r\nconnection: close\r\n\r\n")
                    .as_bytes(),
            );
            head
        });
        (url, handle)
    }

    #[test]
    fn classifies_reachability_and_auth_separately() {
        assert_eq!(classify(200), HealthStatus::Ok);
        assert_eq!(classify(303), HealthStatus::Ok);
        assert_eq!(classify(401), HealthStatus::Auth);
        assert_eq!(classify(403), HealthStatus::Auth);
        assert_eq!(classify(404), HealthStatus::Http);
        assert_eq!(classify(500), HealthStatus::Http);
    }

    #[test]
    fn only_a_rejected_session_asks_for_a_token_again() {
        let report = |status, credential| HealthReport {
            instance_id: Uuid::new_v4(),
            status,
            latency_ms: 1,
            credential,
        };
        assert!(session_rejected(
            AuthMode::DshToken,
            &report(HealthStatus::Auth, Credential::Cookie)
        ));
        assert!(!session_rejected(
            AuthMode::DshToken,
            &report(HealthStatus::Auth, Credential::None)
        ));
        assert!(!session_rejected(
            AuthMode::DshToken,
            &report(HealthStatus::Ok, Credential::Cookie)
        ));
        assert!(!session_rejected(
            AuthMode::None,
            &report(HealthStatus::Auth, Credential::Cookie)
        ));
    }

    #[test]
    fn a_cookie_makes_a_signed_in_probe_read_ok() {
        let (url, server) = serve(200);
        let report = tauri::async_runtime::block_on(probe_one(
            &client(),
            &dsh(url),
            Some("dsh-auth-x=v1.test"),
        ));
        assert_eq!(report.status, HealthStatus::Ok);
        assert_eq!(report.credential, Credential::Cookie);
        // The value stays out of the failure message, so this asserts the match
        // without printing the head.
        assert!(server
            .join()
            .expect("server")
            .contains("cookie: dsh-auth-x=v1.test"));
    }

    #[test]
    fn a_401_without_a_cookie_is_auth_and_sends_none() {
        let (url, server) = serve(401);
        let report = tauri::async_runtime::block_on(probe_one(&client(), &dsh(url), None));
        assert_eq!(report.status, HealthStatus::Auth);
        assert_eq!(report.credential, Credential::None);
        assert!(!server.join().expect("server").contains("\r\ncookie:"));
    }

    #[test]
    fn a_401_with_a_cookie_is_a_rejected_session_not_a_missing_one() {
        let (url, server) = serve(401);
        let report = tauri::async_runtime::block_on(probe_one(
            &client(),
            &dsh(url),
            Some("dsh-auth-x=v1.expired"),
        ));
        assert_eq!(report.status, HealthStatus::Auth);
        assert_eq!(report.credential, Credential::Cookie);
        assert!(server.join().expect("server").contains("\r\ncookie:"));
    }

    #[test]
    fn a_hub_probe_requests_healthz() {
        let (url, server) = serve(200);
        let hub = Instance {
            id: Uuid::new_v4(),
            name: "hub".to_string(),
            kind: InstanceKind::FlowHub,
            url,
            auth: AuthMode::None,
            group: None,
            order: 0,
            notes: String::new(),
        };
        let report = tauri::async_runtime::block_on(probe_one(&client(), &hub, None));
        assert_eq!(report.status, HealthStatus::Ok);
        assert!(server.join().expect("server").contains("get /healthz "));
    }

    #[test]
    fn a_refused_connection_is_unreachable() {
        let listener = TcpListener::bind("127.0.0.1:0").expect("bind");
        let url = format!("http://{}/", listener.local_addr().expect("addr"));
        drop(listener);
        let report = tauri::async_runtime::block_on(probe_one(&client(), &dsh(url), None));
        assert_eq!(report.status, HealthStatus::Unreachable);
    }

    #[test]
    fn the_report_carries_no_cookie_value() {
        let report = HealthReport {
            instance_id: Uuid::new_v4(),
            status: HealthStatus::Ok,
            latency_ms: 4,
            credential: Credential::Cookie,
        };
        let encoded = serde_json::to_string(&report).expect("json");
        assert!(!encoded.contains("dsh-auth"));
        assert!(!encoded.contains("v1."));
    }
}
