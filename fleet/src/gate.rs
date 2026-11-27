//! The instabox sleep gate: do not open SSH to a host that is not running.

use std::time::Duration;

use crate::config::HostConfig;

pub const DEFAULT_API_BASE: &str = "https://api.instabox.sh";
pub const API_BASE_ENV: &str = "GHA_FLEET_INSTABOX_API_BASE";
pub const API_TIMEOUT_SECONDS: u64 = 15;

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum GateOutcome {
    /// No gate configured, or the machine is running.
    Open,
    Asleep(String),
    Error(String),
}

#[derive(Debug, Clone)]
pub struct Gate {
    base: String,
}

impl Gate {
    pub fn from_env() -> Result<Self, String> {
        #[cfg(debug_assertions)]
        if let Ok(value) = std::env::var(API_BASE_ENV) {
            let value = value.trim();
            if !value.is_empty() {
                return Self::new(value);
            }
        }
        Self::new(DEFAULT_API_BASE)
    }

    pub fn new(base: &str) -> Result<Self, String> {
        if !(base.starts_with("https://") || base.starts_with("http://")) {
            return Err(format!(
                "instabox API base {base:?} must start with http:// or https://"
            ));
        }
        if base.contains(char::is_whitespace) {
            return Err(format!("instabox API base {base:?} contains whitespace"));
        }
        Ok(Gate {
            base: base.trim_end_matches('/').to_string(),
        })
    }

    pub fn check(&self, host: &HostConfig) -> GateOutcome {
        let Some(machine_id) = &host.instabox_machine_id else {
            return GateOutcome::Open;
        };
        let key_env = match &host.instabox_api_key_env {
            Some(name) => name,
            None => {
                return GateOutcome::Error(format!(
                    "host {}: instabox_machine_id is set but instabox_api_key_env is missing",
                    host.name
                ))
            }
        };
        let key = match std::env::var(key_env) {
            Ok(key) if !key.trim().is_empty() => key,
            _ => {
                return GateOutcome::Error(format!(
                    "host {}: environment variable {key_env} is missing or empty",
                    host.name
                ))
            }
        };
        let url = format!("{}/v1/machines/{}", self.base, machine_id);
        let config = ureq::Agent::config_builder()
            .timeout_global(Some(Duration::from_secs(API_TIMEOUT_SECONDS)))
            // Never follow redirects: a redirect could carry X-API-Key to a
            // different origin. Seeing every status here also lets us reject
            // non-2xx before reading any response body.
            .http_status_as_error(false)
            .max_redirects(0)
            .build();
        let agent: ureq::Agent = config.into();
        let response = agent.get(&url).header("X-API-Key", &key).call();
        match response {
            Ok(mut response) => {
                let status = response.status();
                if !(200..=299).contains(&status.as_u16()) {
                    return GateOutcome::Error(format!(
                        "host {}: instabox API returned HTTP {}",
                        host.name,
                        status.as_u16()
                    ));
                }
                let body = match response.body_mut().read_to_string() {
                    Ok(body) => body,
                    Err(e) => {
                        return GateOutcome::Error(format!(
                            "host {}: read instabox response: {e}",
                            host.name
                        ))
                    }
                };
                let value: serde_json::Value = match serde_json::from_str(&body) {
                    Ok(value) => value,
                    Err(e) => {
                        return GateOutcome::Error(format!(
                            "host {}: instabox response is not valid JSON: {e}",
                            host.name
                        ))
                    }
                };
                let state = value
                    .get("state")
                    .and_then(|state| state.as_str())
                    .unwrap_or("");
                if state.is_empty() {
                    return GateOutcome::Error(format!(
                        "host {}: instabox response has no string state",
                        host.name
                    ));
                }
                if state == "running" {
                    GateOutcome::Open
                } else {
                    GateOutcome::Asleep(format!("asleep({state})"))
                }
            }
            Err(e) => GateOutcome::Error(format!(
                "host {}: instabox API request failed: {e}",
                host.name
            )),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn validates_base_url() {
        assert!(Gate::new("https://api.instabox.sh").is_ok());
        assert!(Gate::new("http://127.0.0.1:1234/").is_ok());
        assert!(Gate::new("ftp://example.com").is_err());
        assert!(Gate::new("https://a b").is_err());
    }
}
