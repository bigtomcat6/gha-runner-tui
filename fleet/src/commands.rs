//! Command implementations: status, plan, apply, adopt, import, runtime.

use std::io::Write;

use serde_json::{json, Value};

use crate::bundle::BundleState;
use crate::config::{FleetConfig, HostConfig};
use crate::contract::{
    action_summary, failure_message, has_changes, is_rejected, parse_json, parse_status_json,
    ExportOutput, PlanOutput, Reachability, RuntimeOutput,
};
use crate::gate::{Gate, GateOutcome};
use crate::remote::{self, RawReply, RemoteError};
use crate::state::{self, ImportAction, ImportPlan};
use crate::time::{format_age, now_seconds, parse_rfc3339};

pub struct Context {
    pub config: FleetConfig,
    pub expected: state::ExpectedState,
    pub gate: Option<Gate>,
    pub stdin_is_terminal: bool,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum RuntimeOp {
    Mode,
    Start,
    Stop,
    Restart,
}

impl RuntimeOp {
    pub fn node_command(self) -> &'static str {
        match self {
            RuntimeOp::Mode => "mode",
            RuntimeOp::Start => "start",
            RuntimeOp::Stop => "stop",
            RuntimeOp::Restart => "restart",
        }
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum Access {
    Open,
    Asleep,
    Error,
}

fn access(ctx: &Context, host: &HostConfig) -> (Access, Option<String>) {
    match &ctx.gate {
        None => (Access::Open, None),
        Some(gate) => match gate.check(host) {
            GateOutcome::Open => (Access::Open, None),
            GateOutcome::Asleep(message) => (Access::Asleep, Some(message)),
            GateOutcome::Error(message) => (Access::Error, Some(message)),
        },
    }
}

fn reachability_of_remote_error(error: &RemoteError) -> Reachability {
    match error {
        RemoteError::Spawn(message) => {
            Reachability::Unreachable(format!("ssh could not be started locally: {message}"))
        }
        RemoteError::Timeout { seconds, stderr } => Reachability::Unknown(append_stderr(
            format!("ssh timed out after {seconds}s; the result is unknown"),
            stderr,
        )),
        RemoteError::Signal(signal) => match signal {
            Some(signal) => Reachability::Unknown(format!(
                "ssh terminated by signal {signal}; the result is unknown"
            )),
            None => {
                Reachability::Unknown("ssh terminated abnormally; the result is unknown".into())
            }
        },
        RemoteError::Wait(message) => Reachability::Unknown(format!(
            "{message}; the ssh child may have run the command, so the result is unknown"
        )),
        RemoteError::InvalidOutput(message) => {
            Reachability::Unknown(format!("node stdout is invalid: {message}"))
        }
        RemoteError::SshClientFailure { stderr } => Reachability::Unknown(append_stderr(
            format!(
                "ssh exited {}; the remote command may have started and the result is unknown",
                remote::SSH_CLIENT_FAILURE_EXIT_CODE
            ),
            stderr,
        )),
    }
}

fn invalid_json(host: &HostConfig, raw: &RawReply) -> String {
    append_stderr(
        format!(
            "the node did not print a valid JSON object for host {} (exit {}); the result is unknown",
            host.name, raw.exit_code
        ),
        &raw.stderr,
    )
}

fn append_stderr(mut message: String, stderr: &str) -> String {
    if let Some(tail) = remote::stderr_tail(stderr) {
        message.push_str(": ");
        message.push_str(&tail);
    }
    message
}

fn argv_status(host: &HostConfig) -> Vec<String> {
    vec![
        "status".into(),
        "--config".into(),
        host.config.clone(),
        "--json".into(),
        "--no-github".into(),
    ]
}

fn argv_export(host: &HostConfig) -> Vec<String> {
    vec![
        "export".into(),
        "--config".into(),
        host.config.clone(),
        "--owner".into(),
        "fleet".into(),
        "--json".into(),
    ]
}

fn argv_adopt(host: &HostConfig, owner: &str, glob: &str, plan_id: Option<&str>) -> Vec<String> {
    let mut args = vec![
        "adopt".into(),
        "--config".into(),
        host.config.clone(),
        "--owner".into(),
        owner.into(),
        "--json".into(),
    ];
    match plan_id {
        Some(plan_id) => {
            args.push("--expect-plan".into());
            args.push(plan_id.into());
        }
        None => args.push("--dry-run".into()),
    }
    args.push("--match".into());
    args.push(glob.into());
    args
}

fn argv_apply(host: &HostConfig, prune: bool, plan_id: Option<&str>) -> Vec<String> {
    let mut args = vec![
        "apply".into(),
        "--config".into(),
        host.config.clone(),
        "-f".into(),
        "-".into(),
        "--owner".into(),
        "fleet".into(),
        "--json".into(),
    ];
    match plan_id {
        Some(plan_id) => {
            args.push("--expect-plan".into());
            args.push(plan_id.into());
        }
        None => args.push("--dry-run".into()),
    }
    if prune {
        args.push("--prune".into());
    }
    args
}

fn argv_runtime(
    host: &HostConfig,
    op: RuntimeOp,
    glob: &str,
    mode: Option<u8>,
    delay_seconds: Option<u64>,
) -> Vec<String> {
    let mut args = vec![
        op.node_command().into(),
        "--config".into(),
        host.config.clone(),
        "--json".into(),
        "--match".into(),
        glob.into(),
    ];
    if let Some(mode) = mode {
        args.push(mode.to_string());
    }
    if let Some(delay) = delay_seconds {
        args.push("--delay-seconds".into());
        args.push(delay.to_string());
    }
    args
}

// ---------------------------------------------------------------------------
// status

struct StatusHostResult {
    name: String,
    reachability: Reachability,
    status: Option<Value>,
    note: Option<String>,
    /// True when --cached had no snapshot at all. It is not a failure before
    /// serve (T4) has ever run.
    missing: bool,
}

fn status_worker(ctx: &Context, host: &HostConfig) -> StatusHostResult {
    let (access, gate_message) = access(ctx, host);
    if access == Access::Asleep {
        return StatusHostResult {
            name: host.name.clone(),
            reachability: Reachability::Asleep(gate_message.unwrap_or_default()),
            status: None,
            note: None,
            missing: false,
        };
    }
    if access == Access::Error {
        return StatusHostResult {
            name: host.name.clone(),
            reachability: Reachability::Error(gate_message.unwrap_or_default()),
            status: None,
            note: None,
            missing: false,
        };
    }
    match remote::execute(host, &argv_status(host), None) {
        Err(error) => StatusHostResult {
            name: host.name.clone(),
            reachability: reachability_of_remote_error(&error),
            status: None,
            note: None,
            missing: false,
        },
        Ok(raw) => match parse_status_json(&raw.stdout) {
            Err(_) => StatusHostResult {
                name: host.name.clone(),
                reachability: Reachability::Unknown(invalid_json(host, &raw)),
                status: None,
                note: None,
                missing: false,
            },
            Ok(status) => {
                if raw.exit_code == 0 {
                    StatusHostResult {
                        name: host.name.clone(),
                        reachability: Reachability::Ok,
                        status: Some(status),
                        note: None,
                        missing: false,
                    }
                } else {
                    StatusHostResult {
                        name: host.name.clone(),
                        reachability: Reachability::Error(append_stderr(
                            format!("node status exited {}", raw.exit_code),
                            &raw.stderr,
                        )),
                        status: Some(status),
                        note: None,
                        missing: false,
                    }
                }
            }
        },
    }
}

fn status_exit(results: &[StatusHostResult]) -> i32 {
    if results
        .iter()
        .any(|result| result.reachability.is_unknown())
    {
        3
    } else if results.iter().any(|result| {
        matches!(
            result.reachability,
            Reachability::Unreachable(_) | Reachability::Error(_)
        )
    }) {
        1
    } else {
        0
    }
}

pub fn status(ctx: &Context, hosts: &[HostConfig], json_output: bool) -> i32 {
    let results = map_hosts(hosts, |host| status_worker(ctx, host));
    if json_output {
        print_json_hosts(&results);
    } else {
        for result in &results {
            print_status_result(result);
        }
    }
    status_exit(&results)
}

fn print_json_hosts(results: &[StatusHostResult]) {
    let hosts: Vec<Value> = results
        .iter()
        .map(|result| {
            json!({
                "name": result.name,
                "reachability": result.reachability.label(),
                "error": result.reachability.message(),
                "status": result.status,
            })
        })
        .collect();
    println!("{}", json!({ "hosts": hosts }));
}

fn print_status_result(result: &StatusHostResult) {
    if result.reachability.is_ok() {
        if let Some(status) = &result.status {
            println!("{}: ok", result.name);
            print_status_table(status);
            print_profile_errors(status);
        } else {
            println!("{}: ok (no status)", result.name);
        }
    } else {
        println!(
            "{}: {}: {}",
            result.name,
            result.reachability.label(),
            result.reachability.message().unwrap_or("")
        );
    }
    if let Some(note) = &result.note {
        println!("  {note}");
    }
}

fn scalar(value: Option<&Value>) -> String {
    match value {
        Some(Value::String(text)) => text.clone(),
        Some(Value::Bool(flag)) => flag.to_string(),
        Some(Value::Number(number)) => number.to_string(),
        Some(Value::Null) | None => "unknown".into(),
        Some(other) => other.to_string(),
    }
}

fn print_status_table(status: &Value) {
    let Some(profiles) = status.get("profiles").and_then(Value::as_array) else {
        println!("  (no profiles array in node status JSON)");
        return;
    };
    println!("  PROFILE\tMODE\tSERVICE\tLOOP\tCONTAINER\tHEALTH");
    let mut restart_any = false;
    for profile in profiles {
        let name = scalar(profile.get("name"));
        let mode = profile
            .get("admission")
            .and_then(|admission| admission.get("mode"))
            .map(|mode| {
                let base = scalar(Some(mode));
                let delay = profile
                    .get("admission")
                    .and_then(|admission| admission.get("delay_seconds"))
                    .filter(|_| base == "2");
                match delay {
                    Some(delay) => format!("{base}/{}s", scalar(Some(delay))),
                    None => base,
                }
            })
            .unwrap_or_else(|| "unknown".into());
        let active = scalar(
            profile
                .get("service")
                .and_then(|service| service.get("active")),
        );
        let enabled = if profile
            .get("service")
            .and_then(|service| service.get("enabled"))
            .and_then(Value::as_bool)
            .unwrap_or(false)
        {
            "enabled"
        } else {
            "disabled"
        };
        let service = format!("{active}/{enabled}");
        let loop_state = scalar(
            profile
                .get("loop")
                .and_then(|loop_state| loop_state.get("state")),
        );
        let container = scalar(
            profile
                .get("container")
                .and_then(|container| container.get("state")),
        );
        let health = scalar(profile.get("health"));
        let restart = profile
            .get("restart_pending")
            .and_then(Value::as_bool)
            .unwrap_or(false);
        if restart {
            restart_any = true;
        }
        println!(
            "  {name}\t{mode}\t{service}\t{loop_state}\t{container}\t{health}{}",
            if restart { " *" } else { "" }
        );
    }
    if restart_any {
        println!("  * restart pending");
    }
}

fn print_profile_errors(status: &Value) {
    let Some(errors) = status.get("profile_errors").and_then(Value::as_array) else {
        return;
    };
    for error in errors {
        let path = scalar(error.get("path"));
        let message = scalar(error.get("error"));
        println!("  error: {path}: {message}");
    }
}

pub fn status_cached(ctx: &Context, hosts: &[HostConfig], json_output: bool) -> i32 {
    let mut results = Vec::new();
    let mut read_error = false;
    for host in hosts {
        match state::load_snapshot(&ctx.config.state_dir, &host.name) {
            Err(message) => {
                read_error = true;
                results.push(StatusHostResult {
                    name: host.name.clone(),
                    reachability: Reachability::Error(message),
                    status: None,
                    note: None,
                    missing: false,
                });
            }
            Ok(None) => results.push(StatusHostResult {
                name: host.name.clone(),
                reachability: Reachability::Unknown("no snapshot".into()),
                status: None,
                note: Some("no snapshot available (serve has not completed a round)".into()),
                missing: true,
            }),
            Ok(Some(snapshot)) => {
                let reachability = snapshot
                    .last_attempt
                    .as_ref()
                    .map(|attempt| match attempt.reachability.as_str() {
                        "ok" => Reachability::Ok,
                        "asleep" => Reachability::Asleep(
                            attempt.error.clone().unwrap_or_else(|| "asleep".into()),
                        ),
                        "unreachable" => Reachability::Unreachable(
                            attempt
                                .error
                                .clone()
                                .unwrap_or_else(|| "unreachable".into()),
                        ),
                        "unknown" => {
                            Reachability::Unknown(attempt.error.clone().unwrap_or_default())
                        }
                        "error" => Reachability::Error(attempt.error.clone().unwrap_or_default()),
                        other => Reachability::Unknown(format!("snapshot reachability {other:?}")),
                    })
                    .unwrap_or(Reachability::Ok);
                let note = match &snapshot.last_ok {
                    Some(ok) => match parse_rfc3339(&ok.observed_at) {
                        Some(timestamp) => {
                            let age = (now_seconds() - timestamp).max(0);
                            let stale = age > state::STALE_AFTER_SECONDS;
                            format!(
                                "last ok {} ago{}",
                                format_age(age),
                                if stale { " (stale)" } else { "" }
                            )
                        }
                        None => format!("last ok timestamp {:?} is unreadable", ok.observed_at),
                    },
                    None => "no successful snapshot yet".into(),
                };
                results.push(StatusHostResult {
                    name: host.name.clone(),
                    reachability,
                    status: snapshot.last_ok.and_then(|ok| ok.status),
                    note: Some(note),
                    missing: false,
                });
            }
        }
    }
    if json_output {
        print_json_hosts(&results);
    } else {
        for result in &results {
            print_status_result(result);
        }
    }
    if read_error {
        return 1;
    }
    let mut code = 0;
    for result in &results {
        if result.missing {
            continue;
        }
        if result.reachability.is_unknown() {
            code = 3;
        } else if matches!(
            result.reachability,
            Reachability::Unreachable(_) | Reachability::Error(_)
        ) && code < 1
        {
            code = 1;
        }
    }
    code
}

// ---------------------------------------------------------------------------
// plan / apply / adopt

pub enum PlanPreview {
    Ok(PlanOutput),
    Rejected(PlanOutput, String),
    Unknown(String),
    Asleep(String),
    Unreachable(String),
    Error(String),
}

impl PlanPreview {
    fn label(&self) -> &'static str {
        match self {
            PlanPreview::Ok(_) => "ok",
            PlanPreview::Rejected(_, _) => "rejected",
            PlanPreview::Unknown(_) => "unknown",
            PlanPreview::Asleep(_) => "asleep",
            PlanPreview::Unreachable(_) => "unreachable",
            PlanPreview::Error(_) => "error",
        }
    }
}

fn classify_preview(host: &HostConfig, raw: &RawReply) -> PlanPreview {
    let plan: PlanOutput = match parse_json(&raw.stdout) {
        Ok(plan) => plan,
        Err(_) => return PlanPreview::Unknown(invalid_json(host, raw)),
    };
    if plan.phase != "plan" {
        return PlanPreview::Unknown(format!(
            "node returned phase {:?} for a dry-run (exit {}); the result is unknown",
            plan.phase, raw.exit_code
        ));
    }
    if plan.plan_id.trim().is_empty() {
        return PlanPreview::Unknown(
            "node returned a dry-run plan without a plan_id; the result is unknown".into(),
        );
    }
    if plan.complete {
        return PlanPreview::Unknown(
            "node reported complete=true for a dry-run; the result is unknown".into(),
        );
    }
    match raw.exit_code {
        0 if !is_rejected(&plan) => PlanPreview::Ok(plan),
        0 => {
            let message = failure_message(&plan)
                .unwrap_or_else(|| "the node reported a conflict but exited 0".into());
            PlanPreview::Rejected(plan, message)
        }
        1 | 2 => {
            let message = failure_message(&plan)
                .or_else(|| remote::stderr_tail(&raw.stderr))
                .unwrap_or_else(|| "preflight rejected (nothing written)".into());
            PlanPreview::Rejected(plan, message)
        }
        3 => {
            PlanPreview::Unknown("node returned exit 3 for a dry-run; the result is unknown".into())
        }
        _ => PlanPreview::Unknown(append_stderr(
            format!(
                "node returned unexpected exit {} for a dry-run; the result is unknown",
                raw.exit_code
            ),
            &raw.stderr,
        )),
    }
}

fn preview_remote_result(error: RemoteError) -> PlanPreview {
    match reachability_of_remote_error(&error) {
        Reachability::Asleep(message) => PlanPreview::Asleep(message),
        Reachability::Unreachable(message) => PlanPreview::Unreachable(message),
        Reachability::Unknown(message) => PlanPreview::Unknown(message),
        Reachability::Error(message) => PlanPreview::Error(message),
        Reachability::Ok => PlanPreview::Unknown("unexpected remote state".into()),
    }
}

fn plan_preview(ctx: &Context, host: &HostConfig, bundle: &str, prune: bool) -> PlanPreview {
    let (access, gate_message) = access(ctx, host);
    if access == Access::Asleep {
        return PlanPreview::Asleep(gate_message.unwrap_or_default());
    }
    if access == Access::Error {
        return PlanPreview::Error(gate_message.unwrap_or_default());
    }
    let args = argv_apply(host, prune, None);
    match remote::execute(host, &args, Some(bundle)) {
        Ok(raw) => classify_preview(host, &raw),
        Err(error) => preview_remote_result(error),
    }
}

fn adopt_preview(ctx: &Context, host: &HostConfig, owner: &str, glob: &str) -> PlanPreview {
    let (access, gate_message) = access(ctx, host);
    if access == Access::Asleep {
        return PlanPreview::Asleep(gate_message.unwrap_or_default());
    }
    if access == Access::Error {
        return PlanPreview::Error(gate_message.unwrap_or_default());
    }
    let args = argv_adopt(host, owner, glob, None);
    match remote::execute(host, &args, None) {
        Ok(raw) => classify_preview(host, &raw),
        Err(error) => preview_remote_result(error),
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum ExecClass {
    Applied,
    Rejected,
    Partial,
    Unknown,
    Asleep,
    Unreachable,
    Error,
}

struct ExecResult {
    name: String,
    class: ExecClass,
    plan: Option<PlanOutput>,
    message: String,
}

impl ExecClass {
    fn label(self) -> &'static str {
        match self {
            ExecClass::Applied => "applied",
            ExecClass::Rejected => "rejected",
            ExecClass::Partial => "partial",
            ExecClass::Unknown => "unknown",
            ExecClass::Asleep => "asleep",
            ExecClass::Unreachable => "unreachable",
            ExecClass::Error => "error",
        }
    }

    fn is_failure(self) -> bool {
        !matches!(self, ExecClass::Applied)
    }

    fn is_unknown(self) -> bool {
        matches!(self, ExecClass::Partial | ExecClass::Unknown)
    }
}

fn all_done(plan: &PlanOutput) -> bool {
    plan.results.iter().all(|result| result.outcome == "done")
}

fn classify_execute(host: &HostConfig, raw: &RawReply) -> (ExecClass, Option<PlanOutput>, String) {
    let plan: PlanOutput = match parse_json(&raw.stdout) {
        Ok(plan) => plan,
        Err(_) => return (ExecClass::Unknown, None, invalid_json(host, raw)),
    };
    match plan.phase.as_str() {
        "plan" => {
            if plan.complete {
                return (
                    ExecClass::Unknown,
                    Some(plan),
                    "node returned phase=plan with complete=true; the result is unknown".into(),
                );
            }
            if !matches!(raw.exit_code, 1 | 2) {
                return (
                    ExecClass::Unknown,
                    Some(plan),
                    append_stderr(
                        format!(
                            "node returned phase=plan with exit {} for an execute request; \
                             the result is unknown",
                            raw.exit_code
                        ),
                        &raw.stderr,
                    ),
                );
            }
            let message = failure_message(&plan)
                .or_else(|| remote::stderr_tail(&raw.stderr))
                .unwrap_or_else(|| {
                    "the node rejected the plan and wrote nothing (plan changed, conflict, \
                     lock unavailable, or preflight refusal)"
                        .into()
                });
            (ExecClass::Rejected, Some(plan), message)
        }
        "execute" => {
            if plan.complete {
                if raw.exit_code == 0 && all_done(&plan) && !is_rejected(&plan) {
                    (ExecClass::Applied, Some(plan), String::new())
                } else {
                    (
                        ExecClass::Unknown,
                        Some(plan),
                        append_stderr(
                            format!(
                                "node reported complete but exited {} or had non-done results; \
                                 the result is unknown",
                                raw.exit_code
                            ),
                            &raw.stderr,
                        ),
                    )
                }
            } else if raw.exit_code == 3 {
                let message = failure_message(&plan)
                    .unwrap_or_else(|| "node reported partial execution".into());
                (ExecClass::Partial, Some(plan), message)
            } else {
                (
                    ExecClass::Unknown,
                    Some(plan),
                    append_stderr(
                        format!(
                            "node returned exit {} with phase=execute and complete=false; \
                             the result is unknown",
                            raw.exit_code
                        ),
                        &raw.stderr,
                    ),
                )
            }
        }
        _ => (
            ExecClass::Unknown,
            None,
            format!(
                "node returned unknown phase {:?}; the result is unknown",
                plan.phase
            ),
        ),
    }
}

fn execute_plan(
    ctx: &Context,
    host: &HostConfig,
    bundle: &str,
    plan_id: &str,
    prune: bool,
) -> ExecResult {
    let (access, gate_message) = access(ctx, host);
    if access == Access::Asleep {
        return ExecResult {
            name: host.name.clone(),
            class: ExecClass::Asleep,
            plan: None,
            message: gate_message.unwrap_or_default(),
        };
    }
    if access == Access::Error {
        return ExecResult {
            name: host.name.clone(),
            class: ExecClass::Error,
            plan: None,
            message: gate_message.unwrap_or_default(),
        };
    }
    let args = argv_apply(host, prune, Some(plan_id));
    match remote::execute(host, &args, Some(bundle)) {
        Ok(raw) => {
            let (class, plan, message) = classify_execute(host, &raw);
            ExecResult {
                name: host.name.clone(),
                class,
                plan,
                message,
            }
        }
        Err(error) => remote_exec_result(host, &error),
    }
}

fn execute_adopt(
    ctx: &Context,
    host: &HostConfig,
    owner: &str,
    glob: &str,
    plan_id: &str,
) -> ExecResult {
    let (access, gate_message) = access(ctx, host);
    if access == Access::Asleep {
        return ExecResult {
            name: host.name.clone(),
            class: ExecClass::Asleep,
            plan: None,
            message: gate_message.unwrap_or_default(),
        };
    }
    if access == Access::Error {
        return ExecResult {
            name: host.name.clone(),
            class: ExecClass::Error,
            plan: None,
            message: gate_message.unwrap_or_default(),
        };
    }
    let args = argv_adopt(host, owner, glob, Some(plan_id));
    match remote::execute(host, &args, None) {
        Ok(raw) => {
            let (class, plan, message) = classify_execute(host, &raw);
            ExecResult {
                name: host.name.clone(),
                class,
                plan,
                message,
            }
        }
        Err(error) => remote_exec_result(host, &error),
    }
}

fn remote_exec_result(host: &HostConfig, error: &RemoteError) -> ExecResult {
    let reachability = reachability_of_remote_error(error);
    let (class, message) = match reachability {
        Reachability::Asleep(message) => (ExecClass::Asleep, message),
        Reachability::Unreachable(message) => (ExecClass::Unreachable, message),
        Reachability::Unknown(message) => (ExecClass::Unknown, message),
        Reachability::Error(message) => (ExecClass::Error, message),
        Reachability::Ok => (ExecClass::Unknown, "unexpected remote state".into()),
    };
    ExecResult {
        name: host.name.clone(),
        class,
        plan: None,
        message,
    }
}

fn preview_exit(previews: &[(String, PlanPreview)]) -> i32 {
    if previews
        .iter()
        .any(|(_, preview)| matches!(preview, PlanPreview::Unknown(_)))
    {
        3
    } else if previews
        .iter()
        .any(|(_, preview)| !matches!(preview, PlanPreview::Ok(_)))
    {
        1
    } else {
        0
    }
}

fn exec_exit(results: &[ExecResult]) -> i32 {
    if results.iter().any(|result| result.class.is_unknown()) {
        3
    } else if results.iter().any(|result| result.class.is_failure()) {
        1
    } else {
        0
    }
}

struct PreparedHost<'a> {
    host: &'a HostConfig,
    bundle: String,
}

fn read_bundles<'a>(
    ctx: &Context,
    hosts: &'a [HostConfig],
    prune: bool,
) -> Result<(Vec<PreparedHost<'a>>, Vec<String>), String> {
    let mut prepared = Vec::new();
    let mut empty = Vec::new();
    for host in hosts {
        let (state, bundle) = ctx.expected.read_bundle(&host.name)?;
        match state {
            BundleState::Ready => prepared.push(PreparedHost {
                host,
                bundle: bundle.text,
            }),
            BundleState::Missing | BundleState::Empty => empty.push(host.name.clone()),
        }
    }
    if prune && !empty.is_empty() {
        return Err(format!(
            "refusing --prune: expected-state directory is empty or missing for host(s) {}; \
             an empty bundle would delete every fleet-owned profile on that host",
            empty.join(", ")
        ));
    }
    Ok((prepared, empty))
}

pub fn plan(ctx: &Context, hosts: &[HostConfig], prune: bool) -> i32 {
    let (prepared, empty) = match read_bundles(ctx, hosts, prune) {
        Ok(value) => value,
        Err(message) => {
            eprintln!("gha-fleet: {message}");
            return 1;
        }
    };
    for name in &empty {
        println!("{name}: skipped (no expected-state profiles)");
    }
    let previews = map_prepared(&prepared, |prepared| {
        let preview = plan_preview(ctx, prepared.host, &prepared.bundle, prune);
        (prepared.host.name.clone(), preview)
    });
    for (name, preview) in &previews {
        print_preview(name, preview);
    }
    preview_exit(&previews)
}

fn print_preview(name: &str, preview: &PlanPreview) {
    match preview {
        PlanPreview::Ok(plan) => {
            println!("{name}: plan {} ({})", plan.plan_id, action_summary(plan));
            print_plan_rows(plan);
        }
        PlanPreview::Rejected(plan, message) => {
            println!("{name}: rejected: {message}");
            print_plan_rows(plan);
        }
        PlanPreview::Unknown(message)
        | PlanPreview::Asleep(message)
        | PlanPreview::Unreachable(message)
        | PlanPreview::Error(message) => {
            println!("{name}: {}: {message}", preview.label());
        }
    }
}

fn print_plan_rows(plan: &PlanOutput) {
    if plan.results.is_empty() {
        return;
    }
    println!("  ACTION\tRESTART\tOUTCOME\tNAME\tMESSAGE");
    for result in &plan.results {
        println!(
            "  {}\t{}\t{}\t{}\t{}",
            result.action,
            if result.restart_needed { "needed" } else { "-" },
            result.outcome,
            result.name,
            result.message
        );
    }
}

fn eligible_apply(previews: &[(String, PlanPreview)]) -> Vec<(String, String)> {
    previews
        .iter()
        .filter_map(|(name, preview)| match preview {
            PlanPreview::Ok(plan) if has_changes(plan) => {
                Some((name.clone(), plan.plan_id.clone()))
            }
            _ => None,
        })
        .collect()
}

fn confirm(ctx: &Context, action: &str, hosts: &[String]) -> Result<bool, i32> {
    if hosts.is_empty() {
        return Ok(true);
    }
    if !ctx.stdin_is_terminal {
        eprintln!(
            "gha-fleet: stdin is not a terminal; pass --yes to {action} without confirmation"
        );
        return Err(2);
    }
    print!(
        "{action} on {} host(s) ({})? [y/N] ",
        hosts.len(),
        hosts.join(", ")
    );
    let _ = std::io::stdout().flush();
    let mut line = String::new();
    if std::io::stdin().read_line(&mut line).is_err() {
        return Ok(false);
    }
    Ok(matches!(
        line.trim().to_ascii_lowercase().as_str(),
        "y" | "yes"
    ))
}

pub fn apply(ctx: &Context, hosts: &[HostConfig], prune: bool, yes: bool) -> i32 {
    let (prepared, empty) = match read_bundles(ctx, hosts, prune) {
        Ok(value) => value,
        Err(message) => {
            eprintln!("gha-fleet: {message}");
            return 1;
        }
    };
    for name in &empty {
        println!("{name}: skipped (no expected-state profiles)");
    }
    let previews = map_prepared(&prepared, |prepared| {
        let preview = plan_preview(ctx, prepared.host, &prepared.bundle, prune);
        (prepared.host.name.clone(), preview)
    });
    for (name, preview) in &previews {
        print_preview(name, preview);
    }
    let preview_failures = preview_exit(&previews);
    let eligible = eligible_apply(&previews);
    let unchanged: std::collections::BTreeSet<String> = previews
        .iter()
        .filter_map(|(name, preview)| match preview {
            PlanPreview::Ok(plan) if !has_changes(plan) => Some(name.clone()),
            _ => None,
        })
        .collect();
    if eligible.is_empty() {
        for (name, preview) in &previews {
            if let PlanPreview::Ok(plan) = preview {
                println!("{name}: applied (no changes)");
                print_plan_rows(plan);
            }
        }
        return preview_failures;
    }
    if !yes {
        let names: Vec<String> = eligible.iter().map(|(name, _)| name.clone()).collect();
        match confirm(ctx, "apply", &names) {
            Ok(true) => {}
            Ok(false) => {
                eprintln!("gha-fleet: aborted; nothing was executed");
                return preview_failures;
            }
            Err(code) => return code,
        }
    }
    let pid_by_host: std::collections::BTreeMap<String, String> = eligible.into_iter().collect();
    let runnable: Vec<&PreparedHost> = prepared
        .iter()
        .filter(|prepared| {
            pid_by_host.contains_key(&prepared.host.name) || unchanged.contains(&prepared.host.name)
        })
        .collect();
    let results = map_prepared_refs(&runnable, |prepared| {
        match pid_by_host.get(&prepared.host.name) {
            Some(plan_id) => execute_plan(ctx, prepared.host, &prepared.bundle, plan_id, prune),
            None => ExecResult {
                name: prepared.host.name.clone(),
                class: ExecClass::Applied,
                plan: None,
                message: "no changes".into(),
            },
        }
    });
    for result in &results {
        print_exec_result(result, true);
    }
    combine_exit(preview_failures, exec_exit(&results))
}

pub fn adopt(ctx: &Context, hosts: &[HostConfig], glob: &str, owner: &str, yes: bool) -> i32 {
    let previews = map_hosts(hosts, |host| {
        let preview = adopt_preview(ctx, host, owner, glob);
        (host.name.clone(), preview)
    });
    for (name, preview) in &previews {
        print_preview(name, preview);
    }
    let preview_failures = preview_exit(&previews);
    let eligible: Vec<(String, String)> = previews
        .iter()
        .filter_map(|(name, preview)| match preview {
            PlanPreview::Ok(plan) if plan.results.iter().any(|result| result.action == "adopt") => {
                Some((name.clone(), plan.plan_id.clone()))
            }
            _ => None,
        })
        .collect();
    let unchanged: std::collections::BTreeSet<String> = previews
        .iter()
        .filter_map(|(name, preview)| match preview {
            PlanPreview::Ok(plan)
                if !plan.results.iter().any(|result| result.action == "adopt") =>
            {
                Some(name.clone())
            }
            _ => None,
        })
        .collect();
    if eligible.is_empty() {
        for (name, preview) in &previews {
            if let PlanPreview::Ok(plan) = preview {
                println!("{name}: applied (no ownership changes)");
                print_plan_rows(plan);
            }
        }
        return preview_failures;
    }
    if !yes {
        let names: Vec<String> = eligible.iter().map(|(name, _)| name.clone()).collect();
        match confirm(ctx, "adopt", &names) {
            Ok(true) => {}
            Ok(false) => {
                eprintln!("gha-fleet: aborted; nothing was executed");
                return preview_failures;
            }
            Err(code) => return code,
        }
    }
    let pid_by_host: std::collections::BTreeMap<String, String> = eligible.into_iter().collect();
    let runnable: Vec<HostConfig> = hosts
        .iter()
        .filter(|host| pid_by_host.contains_key(&host.name) || unchanged.contains(&host.name))
        .cloned()
        .collect();
    let results = map_hosts(&runnable, |host| match pid_by_host.get(&host.name) {
        Some(plan_id) => execute_adopt(ctx, host, owner, glob, plan_id),
        None => ExecResult {
            name: host.name.clone(),
            class: ExecClass::Applied,
            plan: None,
            message: "no ownership changes".into(),
        },
    });
    for result in &results {
        print_exec_result(result, false);
    }
    combine_exit(preview_failures, exec_exit(&results))
}

fn combine_exit(left: i32, right: i32) -> i32 {
    left.max(right)
}

fn print_exec_result(result: &ExecResult, apply: bool) {
    match result.class {
        ExecClass::Applied => {
            if let Some(plan) = &result.plan {
                println!("{}: applied ({})", result.name, action_summary(plan));
                print_exec_rows(plan);
            } else {
                println!("{}: applied ({})", result.name, result.message);
            }
        }
        ExecClass::Rejected | ExecClass::Partial => {
            println!(
                "{}: {}: {}",
                result.name,
                result.class.label(),
                result.message
            );
            if let Some(plan) = &result.plan {
                print_exec_rows(plan);
            }
        }
        ExecClass::Unknown => {
            println!(
                "{}: unknown: {}; it may have partially executed — run {}",
                result.name,
                result.message,
                follow_up(apply, &result.name)
            );
            if let Some(plan) = &result.plan {
                print_exec_rows(plan);
            }
        }
        ExecClass::Asleep | ExecClass::Unreachable | ExecClass::Error => {
            println!(
                "{}: {}: {}",
                result.name,
                result.class.label(),
                result.message
            );
        }
    }
}

fn follow_up(apply: bool, host: &str) -> String {
    if apply {
        format!("`gha-fleet plan --host {host}` to see the current difference")
    } else {
        format!(
            "`gha-fleet adopt --host {host} --match <glob>` again to preview; \
             only confirm the newest preview"
        )
    }
}

fn print_exec_rows(plan: &PlanOutput) {
    if plan.results.is_empty() {
        return;
    }
    println!("  ACTION\tRESTART\tOUTCOME\tNAME\tMESSAGE");
    for result in &plan.results {
        let restart = if result.restart.is_empty() || result.restart == "none" {
            if result.restart_needed { "needed" } else { "-" }.to_string()
        } else {
            result.restart.clone()
        };
        println!(
            "  {}\t{}\t{}\t{}\t{}",
            result.action, restart, result.outcome, result.name, result.message
        );
    }
}

// ---------------------------------------------------------------------------
// import

struct ExportReply {
    name: String,
    reachability: Reachability,
    output: Option<ExportOutput>,
    exit_code: i32,
}

fn export_worker(ctx: &Context, host: &HostConfig) -> ExportReply {
    let (access, gate_message) = access(ctx, host);
    if access == Access::Asleep {
        return ExportReply {
            name: host.name.clone(),
            reachability: Reachability::Asleep(gate_message.unwrap_or_default()),
            output: None,
            exit_code: 0,
        };
    }
    if access == Access::Error {
        return ExportReply {
            name: host.name.clone(),
            reachability: Reachability::Error(gate_message.unwrap_or_default()),
            output: None,
            exit_code: 0,
        };
    }
    match remote::execute(host, &argv_export(host), None) {
        Err(error) => ExportReply {
            name: host.name.clone(),
            reachability: reachability_of_remote_error(&error),
            output: None,
            exit_code: 0,
        },
        Ok(raw) => match parse_json::<ExportOutput>(&raw.stdout) {
            Err(_) => ExportReply {
                name: host.name.clone(),
                reachability: Reachability::Unknown(invalid_json(host, &raw)),
                output: None,
                exit_code: raw.exit_code,
            },
            Ok(output) => {
                let reachability = if raw.exit_code == 0 {
                    Reachability::Ok
                } else if raw.exit_code == 1 {
                    // Incomplete export is a valid JSON report with exit 1;
                    // import refuses it later because complete=false.
                    Reachability::Ok
                } else if raw.exit_code == 2 {
                    Reachability::Error(append_stderr(
                        "node rejected the export request (exit 2)".into(),
                        &raw.stderr,
                    ))
                } else {
                    Reachability::Unknown(append_stderr(
                        format!(
                            "node returned unexpected exit {} for export; the result is unknown",
                            raw.exit_code
                        ),
                        &raw.stderr,
                    ))
                };
                ExportReply {
                    name: host.name.clone(),
                    reachability,
                    output: Some(output),
                    exit_code: raw.exit_code,
                }
            }
        },
    }
}

pub fn import(ctx: &Context, hosts: &[HostConfig], force: bool) -> i32 {
    let replies = map_hosts(hosts, |host| export_worker(ctx, host));
    let mut unknown = false;
    let mut failed = false;
    for reply in &replies {
        match &reply.reachability {
            Reachability::Ok => {
                if let Some(output) = &reply.output {
                    if !output.complete || !output.errors.is_empty() || reply.exit_code != 0 {
                        println!(
                            "{}: export incomplete or refused (exit {}); nothing written for any host",
                            reply.name, reply.exit_code
                        );
                        for error in &output.errors {
                            println!("  error: {}: {}", error.path, error.error);
                        }
                        failed = true;
                    }
                }
            }
            Reachability::Unknown(message) => {
                println!("{}: unknown: {message}", reply.name);
                unknown = true;
            }
            other => {
                println!(
                    "{}: {}: {}",
                    reply.name,
                    other.label(),
                    other.message().unwrap_or("")
                );
                failed = true;
            }
        }
    }
    if unknown || failed {
        // An incomplete or uncertain export must not become a local manifest:
        // every selected export has to be complete before any file is written.
        return if unknown { 3 } else { 1 };
    }
    let mut plans: Vec<(&HostConfig, ImportPlan)> = Vec::new();
    for reply in &replies {
        let output = match &reply.output {
            Some(output) => output,
            None => continue,
        };
        let host = &ctx.config.hosts[&reply.name];
        match state::plan_import(
            ctx.expected.root_path(),
            &reply.name,
            &output.profiles,
            force,
        ) {
            Ok(plan) => plans.push((host, plan)),
            Err(message) => {
                println!("{}: refusing import: {message}", reply.name);
                return 1;
            }
        }
    }
    for (host, plan) in &plans {
        match state::commit_import(ctx.expected.root_path(), &host.name, plan) {
            Ok(()) => {
                let mut created = 0;
                let mut overwritten = 0;
                let mut unchanged = 0;
                for write in &plan.writes {
                    match write.action {
                        ImportAction::Create => created += 1,
                        ImportAction::Overwrite => overwritten += 1,
                        ImportAction::Unchanged => unchanged += 1,
                    }
                }
                println!(
                    "{}: imported {created} new, {overwritten} overwritten, {unchanged} unchanged",
                    host.name
                );
                if !plan.local_only.is_empty() {
                    println!(
                        "{}: local-only profiles not in export (left alone): {}",
                        host.name,
                        plan.local_only.join(", ")
                    );
                }
            }
            Err(message) => {
                println!("{}: import failed: {message}", host.name);
                return 1;
            }
        }
    }
    0
}

// ---------------------------------------------------------------------------
// runtime

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum RuntimeClass {
    Done,
    Failed,
    Partial,
    Unknown,
    Asleep,
    Unreachable,
    Error,
}

struct RuntimeHostResult {
    name: String,
    class: RuntimeClass,
    output: Option<RuntimeOutput>,
    message: String,
}

fn runtime_worker(
    ctx: &Context,
    host: &HostConfig,
    op: RuntimeOp,
    glob: &str,
    mode: Option<u8>,
    delay_seconds: Option<u64>,
) -> RuntimeHostResult {
    let (access, gate_message) = access(ctx, host);
    if access == Access::Asleep {
        return RuntimeHostResult {
            name: host.name.clone(),
            class: RuntimeClass::Asleep,
            output: None,
            message: gate_message.unwrap_or_default(),
        };
    }
    if access == Access::Error {
        return RuntimeHostResult {
            name: host.name.clone(),
            class: RuntimeClass::Error,
            output: None,
            message: gate_message.unwrap_or_default(),
        };
    }
    let args = argv_runtime(host, op, glob, mode, delay_seconds);
    match remote::execute(host, &args, None) {
        Err(error) => {
            let reachability = reachability_of_remote_error(&error);
            RuntimeHostResult {
                name: host.name.clone(),
                class: match reachability {
                    Reachability::Asleep(_) => RuntimeClass::Asleep,
                    Reachability::Unreachable(_) => RuntimeClass::Unreachable,
                    Reachability::Unknown(_) => RuntimeClass::Unknown,
                    Reachability::Error(_) => RuntimeClass::Error,
                    Reachability::Ok => RuntimeClass::Unknown,
                },
                output: None,
                message: reachability.message().unwrap_or("").to_string(),
            }
        }
        Ok(raw) => match parse_json::<RuntimeOutput>(&raw.stdout) {
            Err(_) => RuntimeHostResult {
                name: host.name.clone(),
                class: RuntimeClass::Unknown,
                output: None,
                message: invalid_json(host, &raw),
            },
            Ok(output) => {
                let failed_result = output
                    .results
                    .iter()
                    .find(|result| result.outcome != "done")
                    .map(|result| {
                        if result.message.is_empty() {
                            format!("{}: {}", result.name, result.outcome)
                        } else {
                            format!("{}: {}", result.name, result.message)
                        }
                    });
                // Exit 3 after an attempted runtime operation is a partial
                // result; exit 1 is a preflight/no-match refusal.
                match raw.exit_code {
                    0 if failed_result.is_none() => RuntimeHostResult {
                        name: host.name.clone(),
                        class: RuntimeClass::Done,
                        output: Some(output),
                        message: String::new(),
                    },
                    0 => RuntimeHostResult {
                        name: host.name.clone(),
                        class: RuntimeClass::Failed,
                        message: failed_result.unwrap_or_default(),
                        output: Some(output),
                    },
                    1 | 2 => RuntimeHostResult {
                        name: host.name.clone(),
                        class: RuntimeClass::Failed,
                        message: failed_result.unwrap_or_else(|| {
                            append_stderr(
                                format!("node reported runtime refusal (exit {})", raw.exit_code),
                                &raw.stderr,
                            )
                        }),
                        output: Some(output),
                    },
                    3 => RuntimeHostResult {
                        name: host.name.clone(),
                        class: RuntimeClass::Partial,
                        message: failed_result.unwrap_or_else(|| {
                            "node reported partial runtime execution (exit 3)".into()
                        }),
                        output: Some(output),
                    },
                    _ => RuntimeHostResult {
                        name: host.name.clone(),
                        class: RuntimeClass::Unknown,
                        message: append_stderr(
                            format!(
                                "node returned unexpected exit {} for a runtime command",
                                raw.exit_code
                            ),
                            &raw.stderr,
                        ),
                        output: Some(output),
                    },
                }
            }
        },
    }
}

pub fn runtime(
    ctx: &Context,
    hosts: &[HostConfig],
    op: RuntimeOp,
    glob: &str,
    mode: Option<u8>,
    delay_seconds: Option<u64>,
) -> i32 {
    let results = map_hosts(hosts, |host| {
        runtime_worker(ctx, host, op, glob, mode, delay_seconds)
    });
    for result in &results {
        print_runtime_result(result);
    }
    runtime_exit(&results)
}

fn print_runtime_result(result: &RuntimeHostResult) {
    match result.class {
        RuntimeClass::Done => {
            let count = result
                .output
                .as_ref()
                .map(|output| output.results.len())
                .unwrap_or(0);
            println!("{}: done ({count} target(s))", result.name);
        }
        RuntimeClass::Failed | RuntimeClass::Partial => {
            println!(
                "{}: {}: {}",
                result.name,
                if result.class == RuntimeClass::Partial {
                    "partial"
                } else {
                    "failed"
                },
                result.message
            );
            if let Some(output) = &result.output {
                for failed in output.results.iter().filter(|r| r.outcome != "done") {
                    println!(
                        "  {}: {}{}",
                        failed.name,
                        failed.outcome,
                        if failed.message.is_empty() {
                            String::new()
                        } else {
                            format!(": {}", failed.message)
                        }
                    );
                }
            }
        }
        RuntimeClass::Unknown => {
            println!(
                "{}: unknown: {}; it may have partially executed — run \
                 `gha-fleet status --host {}`",
                result.name, result.message, result.name
            );
        }
        RuntimeClass::Asleep => println!("{}: asleep: {}", result.name, result.message),
        RuntimeClass::Unreachable => {
            println!("{}: unreachable: {}", result.name, result.message)
        }
        RuntimeClass::Error => println!("{}: error: {}", result.name, result.message),
    }
}

fn runtime_exit(results: &[RuntimeHostResult]) -> i32 {
    if results
        .iter()
        .any(|result| matches!(result.class, RuntimeClass::Unknown | RuntimeClass::Partial))
    {
        3
    } else if results
        .iter()
        .any(|result| result.class != RuntimeClass::Done)
    {
        1
    } else {
        0
    }
}

// ---------------------------------------------------------------------------
// helpers

fn map_hosts<T, F>(hosts: &[HostConfig], f: F) -> Vec<T>
where
    T: Send,
    F: Fn(&HostConfig) -> T + Sync,
{
    std::thread::scope(|scope| {
        let handles: Vec<_> = hosts.iter().map(|host| scope.spawn(|| f(host))).collect();
        handles
            .into_iter()
            .map(|handle| handle.join().expect("fleet worker thread panicked"))
            .collect()
    })
}

fn map_prepared<'a, T, F>(hosts: &[PreparedHost<'a>], f: F) -> Vec<T>
where
    T: Send,
    F: Fn(&PreparedHost<'a>) -> T + Sync,
{
    std::thread::scope(|scope| {
        let handles: Vec<_> = hosts.iter().map(|host| scope.spawn(|| f(host))).collect();
        handles
            .into_iter()
            .map(|handle| handle.join().expect("fleet worker thread panicked"))
            .collect()
    })
}

fn map_prepared_refs<'a, T, F>(hosts: &[&PreparedHost<'a>], f: F) -> Vec<T>
where
    T: Send,
    F: Fn(&PreparedHost<'a>) -> T + Sync,
{
    std::thread::scope(|scope| {
        let handles: Vec<_> = hosts.iter().map(|host| scope.spawn(|| f(host))).collect();
        handles
            .into_iter()
            .map(|handle| handle.join().expect("fleet worker thread panicked"))
            .collect()
    })
}

pub fn selected_gate_note(hosts: &[HostConfig]) -> Option<Vec<String>> {
    let ungated: Vec<String> = hosts
        .iter()
        .filter(|host| host.instabox_machine_id.is_none())
        .map(|host| host.name.clone())
        .collect();
    if ungated.is_empty() {
        None
    } else {
        Some(ungated)
    }
}

pub fn print_gate_note(ungated: &[String]) {
    eprintln!(
        "note: sleep state was not checked for host(s) with no instabox_machine_id: {}",
        ungated.join(", ")
    );
}

pub fn validate_glob(glob: &str) -> Result<(), String> {
    if glob.is_empty() {
        return Err("--match must not be empty".into());
    }
    if glob.starts_with('-') {
        return Err("--match must not start with '-'".into());
    }
    if !glob
        .chars()
        .all(|c| c.is_ascii_alphanumeric() || matches!(c, '.' | '_' | '*' | '?' | '[' | ']' | '-'))
    {
        return Err(format!(
            "--match {glob:?} may contain only [A-Za-z0-9._*?\\[\\]-]"
        ));
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn ssh_255_is_unknown_and_only_local_spawn_failure_is_unreachable() {
        let client_failure = RemoteError::SshClientFailure {
            stderr: "Permission denied (publickey).\n".into(),
        };
        assert!(matches!(
            reachability_of_remote_error(&client_failure),
            Reachability::Unknown(_)
        ));

        let spawn = RemoteError::Spawn("spawn ssh: No such file or directory".into());
        assert!(matches!(
            reachability_of_remote_error(&spawn),
            Reachability::Unreachable(_)
        ));

        let wait = RemoteError::Wait("wait for ssh: interrupted".into());
        assert!(matches!(
            reachability_of_remote_error(&wait),
            Reachability::Unknown(_)
        ));
    }
}
