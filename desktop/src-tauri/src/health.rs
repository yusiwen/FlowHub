//! Reachability probes for the sidebar. A dsh 401 means "reachable, sign-in
//! required", not "down"; the probe therefore has its own status class instead
//! of treating every non-2xx response as a failure.

use crate::model::Instance;
use crate::state::AppState;
use crate::store::lock;
use serde::{Deserialize, Serialize};
use std::sync::atomic::Ordering;
use std::time::{Duration, Instant};
use tauri::{AppHandle, Emitter, Manager};
use uuid::Uuid;

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "kebab-case")]
pub enum HealthStatus {
    Ok,
    Auth,
    Http,
    Unreachable,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct HealthReport {
    pub instance_id: Uuid,
    pub status: HealthStatus,
    pub latency_ms: u64,
}

pub fn classify(code: u16) -> HealthStatus {
    match code {
        200..=399 => HealthStatus::Ok,
        401 | 403 => HealthStatus::Auth,
        _ => HealthStatus::Http,
    }
}

pub async fn probe_all(app: &AppHandle) -> Vec<HealthReport> {
    if app.state::<AppState>().probing.swap(true, Ordering::SeqCst) {
        return Vec::new();
    }

    let instances = {
        let state = app.state::<AppState>();
        let stored = lock(&state.store);
        match stored {
            Ok(guard) => guard.instances().to_vec(),
            Err(_) => {
                state.probing.store(false, Ordering::SeqCst);
                return Vec::new();
            }
        }
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
        let report = probe_one(&client, &instance).await;
        {
            let state = app.state::<AppState>();
            let guard = state.health.lock();
            if let Ok(mut health) = guard {
                health.insert(report.instance_id, report.clone());
            }
        }
        let _ = app.emit("health://changed", report.clone());
        reports.push(report);
    }

    app.state::<AppState>()
        .probing
        .store(false, Ordering::SeqCst);
    reports
}

async fn probe_one(client: &reqwest::Client, instance: &Instance) -> HealthReport {
    let started = Instant::now();
    let status = match client.get(&instance.url).send().await {
        Ok(response) => classify(response.status().as_u16()),
        Err(_) => HealthStatus::Unreachable,
    };
    HealthReport {
        instance_id: instance.id,
        status,
        latency_ms: started.elapsed().as_millis().min(u128::from(u64::MAX)) as u64,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn classifies_reachability_and_auth_separately() {
        assert_eq!(classify(200), HealthStatus::Auth);
        assert_eq!(classify(303), HealthStatus::Ok);
        assert_eq!(classify(401), HealthStatus::Auth);
        assert_eq!(classify(403), HealthStatus::Auth);
        assert_eq!(classify(404), HealthStatus::Http);
        assert_eq!(classify(500), HealthStatus::Http);
    }
}
