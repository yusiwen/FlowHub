//! Read-only view of a FlowHub service's `/healthz`.
//!
//! The dashboard is fed by the same payload the service exposes for local
//! operations. It is parsed here, not in the shell, so the shell never fetches a
//! URL itself (its CSP only allows Tauri IPC) and so a wrong address is reported
//! as "not a FlowHub service" instead of rendering arbitrary JSON.

use crate::model::healthz_url;
use serde::{Deserialize, Serialize};
use serde_json::Value;
use std::time::Duration;

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
}

/// A wrong address must not make the app allocate an arbitrary page.
const MAX_PAYLOAD_BYTES: usize = 1 << 20;

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
            format!("{url} is unreachable: {error}")
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

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::{Read, Write};
    use std::net::TcpListener;
    use std::thread::JoinHandle;

    /// Serve exactly one request with a fixed body and return the head received.
    fn serve(status: u16, body: &'static str) -> (String, JoinHandle<String>) {
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
        let (url, server) = serve(200, PAYLOAD);
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
        let (url, server) = serve(200, PAYLOAD);
        let full = format!("{url}healthz");
        let snapshot = tauri::async_runtime::block_on(fetch(&full)).expect("snapshot");
        assert_eq!(snapshot.status, "ok");
        assert!(server.join().expect("server").contains("get /healthz "));
    }

    #[test]
    fn a_200_that_is_not_a_flowhub_payload_is_refused() {
        let (url, server) = serve(200, r#"{"hello":"world"}"#);
        let error = tauri::async_runtime::block_on(fetch(&url)).expect_err("refused");
        assert!(
            error.contains("did not return a FlowHub payload"),
            "{error}"
        );
        server.join().expect("server");
    }

    #[test]
    fn a_non_json_200_is_refused() {
        let (url, server) = serve(200, "<html>hello</html>");
        let error = tauri::async_runtime::block_on(fetch(&url)).expect_err("refused");
        assert!(
            error.contains("did not return a FlowHub payload"),
            "{error}"
        );
        server.join().expect("server");
    }

    #[test]
    fn an_http_error_carries_its_status() {
        let (url, server) = serve(500, "{}");
        let error = tauri::async_runtime::block_on(fetch(&url)).expect_err("refused");
        assert!(error.contains("HTTP 500"), "{error}");
        server.join().expect("server");
    }

    #[test]
    fn a_status_only_payload_parses_with_defaults() {
        let (url, server) = serve(200, r#"{"status":"ok"}"#);
        let snapshot = tauri::async_runtime::block_on(fetch(&url)).expect("snapshot");
        assert_eq!(snapshot.version, "");
        assert_eq!(snapshot.uptime_seconds, 0);
        server.join().expect("server");
    }
}
