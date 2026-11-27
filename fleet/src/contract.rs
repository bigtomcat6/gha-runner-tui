//! Wire types for the node-center contract and the center's classification of
//! node replies.

use serde::{Deserialize, Serialize};
use serde_json::Value;

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct ExportOutput {
    pub complete: bool,
    pub profiles: Vec<ExportProfile>,
    pub errors: Vec<ExportError>,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct ExportProfile {
    pub name: String,
    pub path: String,
    pub owner: String,
    pub yaml: String,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct ExportError {
    pub path: String,
    pub error: String,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct PlanOutput {
    pub phase: String,
    pub plan_id: String,
    pub complete: bool,
    pub results: Vec<PlanResult>,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct PlanResult {
    pub name: String,
    pub action: String,
    #[serde(default)]
    pub restart_needed: bool,
    pub outcome: String,
    #[serde(default)]
    pub restart: String,
    #[serde(default)]
    pub message: String,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct RuntimeOutput {
    pub results: Vec<RuntimeResult>,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct RuntimeResult {
    pub name: String,
    pub outcome: String,
    #[serde(default)]
    pub message: String,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum CommandKind {
    Status,
    Export,
    Plan,
    ExecutePlan,
}

/// What the center knows about a host after it tried to reach the node.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Reachability {
    Ok,
    Asleep(String),
    Unreachable(String),
    Unknown(String),
    Error(String),
}

impl Reachability {
    pub fn label(&self) -> &'static str {
        match self {
            Reachability::Ok => "ok",
            Reachability::Asleep(_) => "asleep",
            Reachability::Unreachable(_) => "unreachable",
            Reachability::Unknown(_) => "unknown",
            Reachability::Error(_) => "error",
        }
    }

    pub fn message(&self) -> Option<&str> {
        match self {
            Reachability::Ok => None,
            Reachability::Asleep(m)
            | Reachability::Unreachable(m)
            | Reachability::Unknown(m)
            | Reachability::Error(m) => Some(m),
        }
    }

    pub fn is_ok(&self) -> bool {
        matches!(self, Reachability::Ok)
    }

    pub fn is_unknown(&self) -> bool {
        matches!(self, Reachability::Unknown(_))
    }
}

/// The classified outcome of a remote command.
#[derive(Debug, Clone)]
pub enum NodeReply<T> {
    Reply(T),
    Asleep(String),
    Unreachable(String),
    Unknown(String),
    Error(String),
}

impl<T> NodeReply<T> {
    pub fn reachability(&self) -> Reachability {
        match self {
            NodeReply::Reply(_) => Reachability::Ok,
            NodeReply::Asleep(m) => Reachability::Asleep(m.clone()),
            NodeReply::Unreachable(m) => Reachability::Unreachable(m.clone()),
            NodeReply::Unknown(m) => Reachability::Unknown(m.clone()),
            NodeReply::Error(m) => Reachability::Error(m.clone()),
        }
    }
}

/// Parse a contract JSON object, refusing trailing junk.
pub fn parse_json<T: for<'de> Deserialize<'de>>(stdout: &str) -> Result<T, String> {
    let mut de = serde_json::Deserializer::from_str(stdout);
    let value = T::deserialize(&mut de).map_err(|e| format!("invalid JSON: {e}"))?;
    de.end()
        .map_err(|e| format!("trailing data after JSON: {e}"))?;
    Ok(value)
}

/// Parse status output as an opaque value: the center must forward the node's
/// JSON unchanged and only read the documented fields for text rendering.
pub fn parse_status_json(stdout: &str) -> Result<Value, String> {
    parse_json(stdout)
}

pub fn has_changes(plan: &PlanOutput) -> bool {
    plan.results
        .iter()
        .any(|r| !matches!(r.action.as_str(), "unchanged"))
}

pub fn is_rejected(plan: &PlanOutput) -> bool {
    plan.results
        .iter()
        .any(|r| matches!(r.action.as_str(), "conflict" | "error"))
}

pub fn failure_message(plan: &PlanOutput) -> Option<String> {
    for result in &plan.results {
        if matches!(result.outcome.as_str(), "failed" | "not_run")
            || matches!(result.action.as_str(), "conflict" | "error")
        {
            let message = result.message.trim();
            if !message.is_empty() {
                return Some(format!("{}: {}", result.name, message));
            }
            return Some(format!("{}: {}", result.name, result.action));
        }
    }
    None
}

pub fn action_summary(plan: &PlanOutput) -> String {
    let mut creates = 0;
    let mut updates = 0;
    let mut unchanged = 0;
    let mut restarts = 0;
    let mut deletes = 0;
    let mut adopts = 0;
    let mut conflicts = 0;
    let mut errors = 0;
    for r in &plan.results {
        match r.action.as_str() {
            "create" => creates += 1,
            "update" => updates += 1,
            "unchanged" => unchanged += 1,
            "restart" => restarts += 1,
            "delete" => deletes += 1,
            "adopt" => adopts += 1,
            "conflict" => conflicts += 1,
            "error" => errors += 1,
            _ => {}
        }
    }
    let mut parts = Vec::new();
    for (count, label) in [
        (creates, "create"),
        (updates, "update"),
        (restarts, "restart"),
        (deletes, "delete"),
        (adopts, "adopt"),
        (unchanged, "unchanged"),
        (conflicts, "conflict"),
        (errors, "error"),
    ] {
        if count > 0 {
            parts.push(format!("{count} {label}"));
        }
    }
    if parts.is_empty() {
        "empty".to_string()
    } else {
        parts.join(", ")
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn parses_export_fixture() {
        let value = json!({
            "complete": true,
            "profiles": [
                {"name": "a", "path": "/p/a.yaml", "owner": "fleet", "yaml": "name: a\n"}
            ],
            "errors": []
        });
        let parsed: ExportOutput = serde_json::from_value(value).unwrap();
        assert!(parsed.complete);
        assert_eq!(parsed.profiles[0].owner, "fleet");
    }

    #[test]
    fn rejects_trailing_junk() {
        assert!(parse_json::<ExportOutput>(
            "{\"complete\":true,\"profiles\":[],\"errors\":[]}junk"
        )
        .is_err());
    }

    #[test]
    fn detects_changes_and_conflicts() {
        let plan = PlanOutput {
            phase: "plan".into(),
            plan_id: "sha256:x".into(),
            complete: false,
            results: vec![
                PlanResult {
                    name: "a".into(),
                    action: "create".into(),
                    restart_needed: true,
                    outcome: "planned".into(),
                    restart: "none".into(),
                    message: String::new(),
                },
                PlanResult {
                    name: "b".into(),
                    action: "unchanged".into(),
                    restart_needed: false,
                    outcome: "planned".into(),
                    restart: "none".into(),
                    message: String::new(),
                },
            ],
        };
        assert!(has_changes(&plan));
        assert!(!is_rejected(&plan));
        assert_eq!(action_summary(&plan), "1 create, 1 unchanged");
    }
}
