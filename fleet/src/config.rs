//! Loading and validating `<dir>/fleet.toml`.

use serde::Deserialize;
use std::collections::BTreeMap;
use std::fs;
use std::path::{Path, PathBuf};

pub const DEFAULT_TIMEOUT_SECONDS: u64 = 60;
pub const MAX_TIMEOUT_SECONDS: u64 = 3600;

#[derive(Debug, Clone)]
pub struct FleetConfig {
    pub state_dir: PathBuf,
    pub hosts: BTreeMap<String, HostConfig>,
}

#[derive(Debug, Clone)]
pub struct HostConfig {
    pub name: String,
    pub ssh: String,
    pub cli: String,
    pub config: String,
    pub timeout_seconds: u64,
    pub instabox_machine_id: Option<String>,
    pub instabox_api_key_env: Option<String>,
}

#[derive(Debug, Deserialize)]
#[serde(deny_unknown_fields)]
struct FleetFile {
    state_dir: String,
    #[serde(default)]
    hosts: BTreeMap<String, HostFile>,
}

#[derive(Debug, Deserialize)]
#[serde(deny_unknown_fields)]
struct HostFile {
    ssh: String,
    cli: String,
    config: String,
    #[serde(default = "default_timeout")]
    timeout_seconds: u64,
    #[serde(default)]
    instabox_machine_id: Option<String>,
    #[serde(default)]
    instabox_api_key_env: Option<String>,
}

fn default_timeout() -> u64 {
    DEFAULT_TIMEOUT_SECONDS
}

pub fn load(dir: &Path) -> Result<FleetConfig, String> {
    let path = dir.join("fleet.toml");
    let text = fs::read_to_string(&path).map_err(|e| format!("read {}: {e}", path.display()))?;
    parse(&text, &path)
}

pub fn parse(text: &str, origin: &Path) -> Result<FleetConfig, String> {
    let file: FleetFile =
        toml::from_str(text).map_err(|e| format!("parse {}: {e}", origin.display()))?;
    validate(file)
}

fn validate(file: FleetFile) -> Result<FleetConfig, String> {
    if file.hosts.is_empty() {
        return Err("fleet.toml: at least one [hosts.<name>] entry is required".into());
    }
    if !is_absolute_path(&file.state_dir) {
        return Err(format!(
            "fleet.toml: state_dir must be an absolute path, got {:?}",
            file.state_dir
        ));
    }
    let state_dir = PathBuf::from(&file.state_dir);

    let mut hosts = BTreeMap::new();
    for (name, host) in file.hosts {
        validate_host_name(&name)?;
        let ssh = validate_ssh_target(&name, &host.ssh)?;
        validate_remote_path(&name, "cli", &host.cli)?;
        validate_remote_path(&name, "config", &host.config)?;
        if !(1..=MAX_TIMEOUT_SECONDS).contains(&host.timeout_seconds) {
            return Err(format!(
                "host {name:?}: timeout_seconds must be between 1 and {MAX_TIMEOUT_SECONDS}"
            ));
        }
        if let Some(machine_id) = &host.instabox_machine_id {
            validate_machine_id(&name, machine_id)?;
        }
        if let Some(key_env) = &host.instabox_api_key_env {
            validate_env_name(&name, key_env)?;
        }
        hosts.insert(
            name.clone(),
            HostConfig {
                name,
                ssh,
                cli: host.cli,
                config: host.config,
                timeout_seconds: host.timeout_seconds,
                instabox_machine_id: host.instabox_machine_id,
                instabox_api_key_env: host.instabox_api_key_env,
            },
        );
    }
    Ok(FleetConfig { state_dir, hosts })
}

fn validate_host_name(name: &str) -> Result<(), String> {
    if name.is_empty() {
        return Err("fleet.toml: host name must not be empty".into());
    }
    if !name
        .chars()
        .all(|c| c.is_ascii_lowercase() || c.is_ascii_digit() || c == '-')
    {
        return Err(format!(
            "fleet.toml: host name {name:?} must contain only [a-z0-9-]"
        ));
    }
    Ok(())
}

/// The SSH destination is passed to ssh as an argv element. Reject anything
/// that could be interpreted as an option or that needs shell syntax.
fn validate_ssh_target(host: &str, target: &str) -> Result<String, String> {
    if target.is_empty() {
        return Err(format!("host {host:?}: ssh must not be empty"));
    }
    if target.starts_with('-') {
        return Err(format!("host {host:?}: ssh target must not start with '-'"));
    }
    if target.contains("..") {
        return Err(format!("host {host:?}: ssh target must not contain '..'"));
    }
    let mut parts = target.split('@');
    let user = parts.next().unwrap_or("");
    let host_part = parts.next();
    if parts.next().is_some() {
        return Err(format!("host {host:?}: ssh target has more than one '@'"));
    }
    if user.is_empty() {
        return Err(format!("host {host:?}: ssh target has an empty user"));
    }
    if let Some(host_part) = host_part {
        if host_part.is_empty() {
            return Err(format!("host {host:?}: ssh target has an empty host"));
        }
        validate_ssh_component(host, host_part)?;
    }
    validate_ssh_component(host, user)?;
    Ok(target.to_string())
}

fn validate_ssh_component(host: &str, part: &str) -> Result<(), String> {
    if part.starts_with('-') {
        return Err(format!(
            "host {host:?}: ssh target component must not start with '-'"
        ));
    }
    if !part
        .chars()
        .all(|c| c.is_ascii_alphanumeric() || matches!(c, '.' | '_' | '-'))
    {
        return Err(format!(
            "host {host:?}: ssh target {part:?} contains unsafe characters"
        ));
    }
    Ok(())
}

fn is_absolute_path(path: &str) -> bool {
    Path::new(path).is_absolute()
}

fn validate_remote_path(host: &str, field: &str, path: &str) -> Result<(), String> {
    if !is_absolute_path(path) {
        return Err(format!("host {host:?}: {field} must be an absolute path"));
    }
    if path.contains("..") {
        return Err(format!("host {host:?}: {field} must not contain '..'"));
    }
    if !path
        .chars()
        .all(|c| c.is_ascii_alphanumeric() || matches!(c, '.' | '_' | '-' | '/'))
    {
        return Err(format!(
            "host {host:?}: {field} may contain only [A-Za-z0-9._/-]"
        ));
    }
    Ok(())
}

fn validate_machine_id(host: &str, machine_id: &str) -> Result<(), String> {
    if machine_id.is_empty()
        || !machine_id
            .chars()
            .all(|c| c.is_ascii_alphanumeric() || matches!(c, '_' | '-'))
    {
        return Err(format!(
            "host {host:?}: instabox_machine_id may contain only [A-Za-z0-9_-]"
        ));
    }
    Ok(())
}

fn validate_env_name(host: &str, name: &str) -> Result<(), String> {
    let mut chars = name.chars();
    let first = chars.next().unwrap_or('\0');
    if !(first.is_ascii_alphabetic() || first == '_')
        || !chars.all(|c| c.is_ascii_alphanumeric() || c == '_')
    {
        return Err(format!(
            "host {host:?}: instabox_api_key_env {name:?} is not a valid environment variable name"
        ));
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    fn valid() -> String {
        r#"
state_dir = "/var/lib/gha-fleet"

[hosts.s1]
ssh = "instabox-s1"
cli = "/data/gha-runner/tools/4469c7b/bin/gha-runner-tui"
config = "/etc/gha-runner-tui/config-live-20261117.yaml"
timeout_seconds = 60
instabox_machine_id = "m_agqyar3gtvzv3znj7d3artwyqq"
instabox_api_key_env = "INSTABOX_S1_API_KEY"

[hosts.s2]
ssh = "ubuntu@tokyo-s2"
cli = "/opt/gha-runner-tui/4469c7b/bin/gha-runner-tui"
config = "/etc/gha-runner-tui/config-live-20261117.yaml"
"#
        .into()
    }

    #[test]
    fn parses_valid_config_and_defaults_timeout() {
        let cfg = parse(&valid(), Path::new("fleet.toml")).unwrap();
        assert_eq!(cfg.hosts.len(), 2);
        assert_eq!(cfg.hosts["s1"].timeout_seconds, 60);
        assert_eq!(cfg.hosts["s2"].timeout_seconds, 60);
        assert_eq!(cfg.state_dir, PathBuf::from("/var/lib/gha-fleet"));
    }

    #[test]
    fn rejects_ssh_targets_that_look_like_options() {
        for target in ["-oProxyCommand=sh", "user@-oProxyCommand=sh", "-host"] {
            let text = valid().replace("ssh = \"instabox-s1\"", &format!("ssh = {target:?}"));
            assert!(parse(&text, Path::new("fleet.toml")).is_err(), "{target}");
        }
    }

    #[test]
    fn rejects_unsafe_ssh_targets() {
        for target in ["host name", "host;rm", "user@@host", "@/host", "user@"] {
            let text = valid().replace("ssh = \"instabox-s1\"", &format!("ssh = {target:?}"));
            assert!(parse(&text, Path::new("fleet.toml")).is_err(), "{target}");
        }
    }

    #[test]
    fn rejects_relative_or_unsafe_remote_paths() {
        let text = valid().replace(
            "cli = \"/data/gha-runner/tools/4469c7b/bin/gha-runner-tui\"",
            "cli = \"bin/gha-runner-tui\"",
        );
        assert!(parse(&text, Path::new("fleet.toml")).is_err());
        let text = valid().replace(
            "config = \"/etc/gha-runner-tui/config-live-20261117.yaml\"",
            "config = \"/etc/gha-runner-tui/config;bad.yaml\"",
        );
        assert!(parse(&text, Path::new("fleet.toml")).is_err());
        let text = valid().replace(
            "config = \"/etc/gha-runner-tui/config-live-20261117.yaml\"",
            "config = \"/etc/../config.yaml\"",
        );
        assert!(parse(&text, Path::new("fleet.toml")).is_err());
    }

    #[test]
    fn rejects_bad_host_names_and_env_names() {
        let text = valid().replace("[hosts.s1]", "[hosts.S1]");
        assert!(parse(&text, Path::new("fleet.toml")).is_err());
        let text = valid().replace(
            "instabox_api_key_env = \"INSTABOX_S1_API_KEY\"",
            "instabox_api_key_env = \"INSTABOX-S1-KEY\"",
        );
        assert!(parse(&text, Path::new("fleet.toml")).is_err());
        let text = valid().replace("timeout_seconds = 60", "timeout_seconds = 0");
        assert!(parse(&text, Path::new("fleet.toml")).is_err());
        let text = valid().replace(
            "instabox_machine_id = \"m_agqyar3gtvzv3znj7d3artwyqq\"",
            "instabox_machine_id = \"m/x\"",
        );
        assert!(parse(&text, Path::new("fleet.toml")).is_err());
    }

    #[test]
    fn rejects_unknown_fields_and_missing_hosts() {
        let text = format!("{}\nmystery = 1\n", valid());
        assert!(parse(&text, Path::new("fleet.toml")).is_err());
        let text = format!("{}\n[hosts.s1]\nunexpected = true\n", valid());
        assert!(parse(&text, Path::new("fleet.toml")).is_err());
        let text = "state_dir = \"/tmp/x\"\n".to_string();
        assert!(parse(&text, Path::new("fleet.toml")).is_err());
    }
}
