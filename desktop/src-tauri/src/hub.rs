//! Read-only view of a FlowHub service's `/healthz`.
//!
//! The dashboard is fed by the same payload the service exposes for local
//! operations. It is parsed here, not in the shell, so the shell never fetches a
//! URL itself (its CSP only allows Tauri IPC) and so a wrong address is reported
//! as "not a FlowHub service" instead of rendering arbitrary JSON.

use crate::model::healthz_url;
use serde::{Deserialize, Serialize};
use serde_json::Value;
use std::error::Error as _;
use std::fs::File;
use std::io::{ErrorKind, Read, Seek, SeekFrom};
use std::time::Duration;
use url::Url;

/// The service's `/healthz` payload. Scalars are typed; the four nested reports
/// stay `Value` because their keys are runtime names and rejection reasons.
/// Unknown fields are ignored, so a newer service still parses.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct HubSnapshot {
    pub status: String,
    #[serde(default)]
    pub version: String,
    #[serde(default)]
    pub uptime_seconds: i64,
    #[serde(default)]
    pub addr: String,
    #[serde(default)]
    pub data_dir: String,
    #[serde(default)]
    pub listens_on_all_interfaces: bool,
    #[serde(default)]
    pub audit_file: String,
    #[serde(default)]
    pub payload_log: String,
    #[serde(default)]
    pub app_log: String,
    #[serde(default)]
    pub queue_depth: i64,
    #[serde(default)]
    pub queue_capacity: i64,
    #[serde(default)]
    pub dedupe_keys: i64,
    #[serde(default)]
    pub projects: Value,
    #[serde(default)]
    pub dispatch: Value,
    #[serde(default)]
    pub runtimes: Value,
    #[serde(default)]
    pub counters: Value,
    #[serde(default)]
    pub flows: Value,
}

/// A wrong address must not make the app allocate an arbitrary page.
const MAX_PAYLOAD_BYTES: usize = 1 << 20;

/// Why a request could not be sent, in the words of the layers that tried.
///
/// `reqwest::Error`'s own message names the request — "error sending request for
/// uri (…)" — and puts the reason (connection refused, no route to host, DNS, TLS)
/// in its `source` chain. Printing the error itself therefore told the operator
/// nothing they could act on: measured 2026-10-21, a service bound to an address
/// the host no longer owned produced a dashboard that said only "error sending
/// request". The chain is joined instead, which ends in the layer that knows.
fn unreachable_reason(error: &reqwest::Error) -> String {
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

pub async fn fetch(base: &str) -> Result<HubSnapshot, String> {
    let url = healthz_url(base)?;
    let client = reqwest::Client::builder()
        .timeout(Duration::from_secs(3))
        .redirect(reqwest::redirect::Policy::none())
        .build()
        .map_err(|error| format!("build the HTTP client: {error}"))?;

    let mut response = client.get(url.clone()).send().await.map_err(|error| {
        if error.is_timeout() {
            format!("{url} timed out")
        } else {
            format!("{url} is unreachable: {}", unreachable_reason(&error))
        }
    })?;

    let status = response.status();
    if !status.is_success() {
        return Err(format!("{url} answered HTTP {}", status.as_u16()));
    }

    let mut body = Vec::new();
    while let Some(chunk) = response
        .chunk()
        .await
        .map_err(|error| format!("read {url}: {error}"))?
    {
        if body.len() + chunk.len() > MAX_PAYLOAD_BYTES {
            return Err(format!(
                "{url} answered with more than {MAX_PAYLOAD_BYTES} bytes"
            ));
        }
        body.extend_from_slice(&chunk);
    }

    let value: Value = serde_json::from_slice(&body)
        .map_err(|_| format!("{url} did not return a FlowHub payload"))?;
    if value.get("status").and_then(Value::as_str).is_none() {
        return Err(format!("{url} did not return a FlowHub payload"));
    }
    serde_json::from_value(value).map_err(|error| format!("{url}: unexpected payload: {error}"))
}

/// The log tail: the last 256 KiB, at most 1000 lines.
pub const LOG_TAIL_BYTES: u64 = 256 * 1024;
pub const MAX_LOG_LINES: usize = 1000;
pub const DEFAULT_LOG_LINES: usize = 200;

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize)]
#[serde(rename_all = "kebab-case")]
pub enum LogState {
    Ok,
    NeedsPath,
    Disabled,
    Missing,
    Unreadable,
}

#[derive(Debug, Clone, Serialize)]
pub struct HubLogs {
    pub state: LogState,
    /// The path actually read (empty when nothing was read).
    pub path: String,
    /// What `/healthz` reports, shown even when it is not trusted to open.
    pub service_path: String,
    /// True when the service is loopback and reports a usable path.
    pub can_use_service_path: bool,
    pub lines: Vec<String>,
    pub truncated: bool,
    pub detail: String,
}

fn usable_service_path(path: &str) -> bool {
    !path.is_empty() && path != "-" && path != "none"
}

/// A loopback service runs on this machine as this user, so its own `app_log`
/// may be read. Any other address may only use a path the user confirmed: a
/// remote endpoint must not be able to make the app display a local file.
pub fn resolve_log_path(base: &str, service_path: &str, confirmed: Option<&str>) -> Option<String> {
    if let Some(path) = confirmed.map(str::trim).filter(|path| !path.is_empty()) {
        return Some(path.to_string());
    }
    if is_loopback(base) && usable_service_path(service_path) {
        return Some(service_path.to_string());
    }
    None
}

pub fn is_loopback(base: &str) -> bool {
    let Ok(url) = Url::parse(base.trim()) else {
        return false;
    };
    match url.host() {
        Some(url::Host::Ipv4(address)) => address.is_loopback(),
        Some(url::Host::Ipv6(address)) => address.is_loopback(),
        // `localhost` is the user's intent even if /etc/hosts points it
        // elsewhere; the path still came from a service on this machine in
        // every configuration the app supports.
        Some(url::Host::Domain(name)) => name.eq_ignore_ascii_case("localhost"),
        None => false,
    }
}

/// Read the end of the file. The first line is dropped when the window starts
/// mid-file, so a returned line is always a whole log record.
fn read_tail(path: &str, wanted: usize) -> std::io::Result<(Vec<String>, bool)> {
    let mut file = File::open(path)?;
    let length = file.metadata()?.len();
    let start = length.saturating_sub(LOG_TAIL_BYTES);
    if start > 0 {
        file.seek(SeekFrom::Start(start))?;
    }
    let mut buffer = Vec::new();
    file.read_to_end(&mut buffer)?;

    let mut text = String::from_utf8_lossy(&buffer).into_owned();
    if start > 0 {
        match text.find('\n') {
            Some(index) => {
                text.drain(..=index);
            }
            None => text.clear(),
        }
    }
    let mut lines: Vec<String> = text.lines().map(ToOwned::to_owned).collect();
    let truncated = start > 0 || lines.len() > wanted;
    if lines.len() > wanted {
        lines.drain(..lines.len() - wanted);
    }
    Ok((lines, truncated))
}

pub async fn logs(base: &str, confirmed: Option<&str>, wanted: usize) -> Result<HubLogs, String> {
    let snapshot = fetch(base).await?;
    let can_use_service_path = is_loopback(base) && usable_service_path(&snapshot.app_log);
    let mut report = HubLogs {
        state: LogState::NeedsPath,
        path: String::new(),
        service_path: snapshot.app_log.clone(),
        can_use_service_path,
        lines: Vec::new(),
        truncated: false,
        detail: String::new(),
    };

    let Some(path) = resolve_log_path(base, &snapshot.app_log, confirmed) else {
        report.state = if matches!(snapshot.app_log.as_str(), "-" | "none") {
            LogState::Disabled
        } else {
            LogState::NeedsPath
        };
        return Ok(report);
    };
    report.path = path.clone();

    match read_tail(&path, wanted.clamp(1, MAX_LOG_LINES)) {
        Ok((lines, truncated)) => {
            report.state = LogState::Ok;
            report.lines = lines;
            report.truncated = truncated;
        }
        Err(error) if error.kind() == ErrorKind::NotFound => {
            report.state = LogState::Missing;
            report.detail = error.to_string();
        }
        Err(error) => {
            report.state = LogState::Unreadable;
            report.detail = error.to_string();
        }
    }
    Ok(report)
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::{Read, Write};
    use std::net::TcpListener;
    use std::thread::JoinHandle;

    /// Serve exactly one request with a fixed body and return the head received.
    fn serve(status: u16, body: String) -> (String, JoinHandle<String>) {
        let listener = TcpListener::bind("127.0.0.1:0").expect("bind");
        let url = format!("http://{}/", listener.local_addr().expect("addr"));
        let handle = std::thread::spawn(move || {
            let (mut stream, _) = listener.accept().expect("accept");
            let mut buffer = [0u8; 8192];
            let read = stream.read(&mut buffer).expect("read");
            let head = String::from_utf8_lossy(&buffer[..read]).to_lowercase();
            let _ = stream.write_all(
                format!(
                    "HTTP/1.1 {status} X\r\ncontent-type: application/json\r\ncontent-length: {}\r\nconnection: close\r\n\r\n{body}",
                    body.len()
                )
                .as_bytes(),
            );
            head
        });
        (url, handle)
    }

    const PAYLOAD: &str = r#"{
        "status": "ok",
        "version": "0.2.1-rc.2",
        "uptime_seconds": 42,
        "addr": "127.0.0.1:8080",
        "data_dir": "./data",
        "listens_on_all_interfaces": false,
        "audit_file": "/tmp/audit.jsonl",
        "payload_log": "/tmp/payload.log",
        "app_log": "/tmp/flowhub.log",
        "queue_depth": 1,
        "queue_capacity": 256,
        "dedupe_keys": 3,
        "projects": {"file": "/tmp/config.json", "mappings": 2, "routable": 1},
        "dispatch": {"enabled": true, "queued": 1, "paused": false},
        "runtimes": {"enabled": true, "counts": {"ready": 1}},
        "counters": {"received": 5, "accepted": 4}
    }"#;

    #[test]
    fn a_healthz_payload_becomes_a_snapshot() {
        let (url, server) = serve(200, PAYLOAD.to_string());
        let snapshot = tauri::async_runtime::block_on(fetch(&url)).expect("snapshot");
        assert_eq!(snapshot.status, "ok");
        assert_eq!(snapshot.version, "0.2.1-rc.2");
        assert_eq!(snapshot.uptime_seconds, 42);
        assert_eq!(snapshot.app_log, "/tmp/flowhub.log");
        assert_eq!(snapshot.queue_capacity, 256);
        assert_eq!(snapshot.projects["mappings"], 2);
        assert_eq!(snapshot.counters["received"], 5);
        assert!(server.join().expect("server").contains("get /healthz "));
    }

    #[test]
    fn a_full_healthz_url_is_not_doubled() {
        let (url, server) = serve(200, PAYLOAD.to_string());
        let full = format!("{url}healthz");
        let snapshot = tauri::async_runtime::block_on(fetch(&full)).expect("snapshot");
        assert_eq!(snapshot.status, "ok");
        assert!(server.join().expect("server").contains("get /healthz "));
    }

    #[test]
    fn a_200_that_is_not_a_flowhub_payload_is_refused() {
        let (url, server) = serve(200, r#"{"hello":"world"}"#.to_string());
        let error = tauri::async_runtime::block_on(fetch(&url)).expect_err("refused");
        assert!(
            error.contains("did not return a FlowHub payload"),
            "{error}"
        );
        server.join().expect("server");
    }

    #[test]
    fn a_non_json_200_is_refused() {
        let (url, server) = serve(200, "<html>hello</html>".to_string());
        let error = tauri::async_runtime::block_on(fetch(&url)).expect_err("refused");
        assert!(
            error.contains("did not return a FlowHub payload"),
            "{error}"
        );
        server.join().expect("server");
    }

    #[test]
    fn an_http_error_carries_its_status() {
        let (url, server) = serve(500, "{}".to_string());
        let error = tauri::async_runtime::block_on(fetch(&url)).expect_err("refused");
        assert!(error.contains("HTTP 500"), "{error}");
        server.join().expect("server");
    }

    #[test]
    fn an_unreachable_service_names_the_reason() {
        // A port nobody listens on: bind, read the port, let it go. The point is not
        // the number but that the reason reaches the panel — "error sending request"
        // alone said nothing about a service bound to an address the host had lost.
        let port = {
            let listener = std::net::TcpListener::bind("127.0.0.1:0").expect("bind");
            listener.local_addr().expect("addr").port()
        };
        let error = tauri::async_runtime::block_on(fetch(&format!("http://127.0.0.1:{port}")))
            .expect_err("nothing listens there");
        assert!(
            error.contains(&format!(":{port}/healthz is unreachable: ")),
            "{error}"
        );
        assert!(
            error.to_lowercase().contains("refused"),
            "the OS reason must reach the panel: {error}"
        );
    }

    #[test]
    fn a_status_only_payload_parses_with_defaults() {
        let (url, server) = serve(200, r#"{"status":"ok"}"#.to_string());
        let snapshot = tauri::async_runtime::block_on(fetch(&url)).expect("snapshot");
        assert_eq!(snapshot.version, "");
        assert_eq!(snapshot.uptime_seconds, 0);
        server.join().expect("server");
    }

    #[test]
    fn loopback_is_the_trust_boundary() {
        for base in [
            "http://127.0.0.1:8080/",
            "http://127.9.9.9:1/",
            "http://[::1]:8080/",
            "http://localhost:8080/",
            "http://LOCALHOST:1/",
        ] {
            assert!(is_loopback(base), "{base}");
        }
        for base in [
            "http://10.1.2.3:8080/",
            "https://youtrack.example.com/",
            "http://[::2]/",
            "not a url",
        ] {
            assert!(!is_loopback(base), "{base}");
        }
    }

    #[test]
    fn a_remote_service_path_is_never_opened() {
        let remote = "http://10.1.2.3:8080/";
        assert_eq!(
            resolve_log_path(remote, "/Users/you/.ssh/id_rsa", None),
            None
        );
        assert_eq!(resolve_log_path(remote, "/var/log/flowhub.log", None), None);
        // The user's own choice is the only thing that opens a remote path.
        assert_eq!(
            resolve_log_path(remote, "/var/log/flowhub.log", Some("/tmp/local.log")),
            Some("/tmp/local.log".to_string())
        );
    }

    #[test]
    fn a_loopback_service_path_is_used_and_an_explicit_choice_wins() {
        let local = "http://127.0.0.1:8080/";
        assert_eq!(
            resolve_log_path(local, "/tmp/flowhub.log", None),
            Some("/tmp/flowhub.log".to_string())
        );
        assert_eq!(
            resolve_log_path(local, "/tmp/flowhub.log", Some("/tmp/other.log")),
            Some("/tmp/other.log".to_string())
        );
        assert_eq!(resolve_log_path(local, "-", None), None);
        assert_eq!(resolve_log_path(local, "none", None), None);
        assert_eq!(resolve_log_path(local, "", None), None);
        assert_eq!(
            resolve_log_path(local, "/tmp/flowhub.log", Some("  ")),
            Some("/tmp/flowhub.log".to_string())
        );
    }

    #[test]
    fn a_tail_is_bounded_and_reported_as_truncated() {
        let dir = tempfile::tempdir().expect("tempdir");
        let path = dir.path().join("flowhub.log");
        let body: String = (1..=300).map(|n| format!("line {n}\n")).collect();
        std::fs::write(&path, body).expect("write");

        let (lines, truncated) = read_tail(path.to_str().expect("path"), 200).expect("read");
        assert_eq!(lines.len(), 200);
        assert!(truncated);
        assert_eq!(lines.last().expect("last"), "line 300");
        assert_eq!(lines.first().expect("first"), "line 101");
    }

    #[test]
    fn a_second_read_sees_an_appended_line() {
        let dir = tempfile::tempdir().expect("tempdir");
        let path = dir.path().join("flowhub.log");
        std::fs::write(&path, "first\n").expect("write");
        let (lines, _) = read_tail(path.to_str().expect("path"), 200).expect("read");
        assert_eq!(lines, vec!["first".to_string()]);

        std::fs::write(&path, "first\nsecond\n").expect("append");
        let (lines, _) = read_tail(path.to_str().expect("path"), 200).expect("read");
        assert_eq!(lines, vec!["first".to_string(), "second".to_string()]);
    }

    #[test]
    fn a_small_tail_is_not_truncated() {
        let dir = tempfile::tempdir().expect("tempdir");
        let path = dir.path().join("flowhub.log");
        std::fs::write(&path, "one\ntwo\n").expect("write");
        let (lines, truncated) = read_tail(path.to_str().expect("path"), 200).expect("read");
        assert_eq!(lines, vec!["one".to_string(), "two".to_string()]);
        assert!(!truncated);
    }

    fn payload_with_log(path: &str) -> String {
        PAYLOAD.replace("/tmp/flowhub.log", path)
    }

    #[test]
    fn a_loopback_hub_tail_is_read() {
        let dir = tempfile::tempdir().expect("tempdir");
        let log = dir.path().join("flowhub.log");
        std::fs::write(&log, "alpha\nbeta\n").expect("write");
        let (url, server) = serve(200, payload_with_log(log.to_str().expect("path")));
        let report = tauri::async_runtime::block_on(logs(&url, None, 200)).expect("logs");
        assert_eq!(report.state, LogState::Ok);
        assert_eq!(report.lines, vec!["alpha".to_string(), "beta".to_string()]);
        assert!(report.can_use_service_path);
        assert!(!report.truncated);
        server.join().expect("server");
    }

    #[test]
    fn a_disabled_log_and_a_missing_file_are_states() {
        let (url, server) = serve(200, PAYLOAD.replace("/tmp/flowhub.log", "-"));
        let report = tauri::async_runtime::block_on(logs(&url, None, 200)).expect("logs");
        assert_eq!(report.state, LogState::Disabled);
        assert!(report.lines.is_empty());
        server.join().expect("server");

        let dir = tempfile::tempdir().expect("tempdir");
        let missing = dir.path().join("not-yet.log");
        let (url, server) = serve(200, payload_with_log(missing.to_str().expect("path")));
        let report = tauri::async_runtime::block_on(logs(&url, None, 200)).expect("logs");
        assert_eq!(report.state, LogState::Missing);
        server.join().expect("server");
    }

    #[test]
    fn a_confirmed_path_beats_the_service_path() {
        let dir = tempfile::tempdir().expect("tempdir");
        let log = dir.path().join("local.log");
        std::fs::write(&log, "chosen\n").expect("write");
        let (url, server) = serve(200, PAYLOAD.to_string());
        let report =
            tauri::async_runtime::block_on(logs(&url, Some(log.to_str().expect("path")), 200))
                .expect("logs");
        assert_eq!(report.state, LogState::Ok);
        assert_eq!(report.lines, vec!["chosen".to_string()]);
        server.join().expect("server");
    }
}
