//! Cross-checks against T1's Go-generated contract goldens.
//!
//! The golden directory must exist; a missing default or `GOLDEN_DIR` fails
//! the test instead of skipping it. The default is
//! `../src/cmd/gha-runner-tui/testdata/contract` relative to this crate. To
//! point at another checkout:
//!
//! ```text
//! GOLDEN_DIR=../src/cmd/gha-runner-tui/testdata/contract cargo test --test golden_contract
//! ```
//!
//! Every scenario must decode with the center's contract types, match the
//! expected phase/exit code below, and pass through the fake-ssh center flows.

mod common;

use common::{FakeHost, TestEnv};
use gha_fleet::contract::{ExportOutput, PlanOutput, RuntimeOutput};
use serde_json::Value;
use std::env;
use std::fs;
use std::path::{Path, PathBuf};

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum Kind {
    Export,
    Plan,
    Runtime,
}

struct Expectation {
    scenario: &'static str,
    kind: Kind,
    exit_code: i32,
}

const EXPECTATIONS: &[Expectation] = &[
    Expectation {
        scenario: "export-complete",
        kind: Kind::Export,
        exit_code: 0,
    },
    Expectation {
        scenario: "export-incomplete",
        kind: Kind::Export,
        exit_code: 1,
    },
    Expectation {
        scenario: "plan-mixed",
        kind: Kind::Plan,
        exit_code: 0,
    },
    Expectation {
        scenario: "plan-conflict",
        kind: Kind::Plan,
        exit_code: 1,
    },
    Expectation {
        scenario: "plan-stale",
        kind: Kind::Plan,
        exit_code: 1,
    },
    Expectation {
        scenario: "execute-ok",
        kind: Kind::Plan,
        exit_code: 0,
    },
    Expectation {
        scenario: "execute-partial",
        kind: Kind::Plan,
        exit_code: 3,
    },
    Expectation {
        scenario: "adopt-ok",
        kind: Kind::Plan,
        exit_code: 0,
    },
    Expectation {
        scenario: "match-mode",
        kind: Kind::Runtime,
        exit_code: 0,
    },
];

fn code(output: &std::process::Output) -> i32 {
    output.status.code().expect("exit code")
}

fn stderr(output: &std::process::Output) -> String {
    String::from_utf8_lossy(&output.stderr).into_owned()
}

fn stdout(output: &std::process::Output) -> String {
    String::from_utf8_lossy(&output.stdout).into_owned()
}

fn golden_dir() -> PathBuf {
    let (path, source) = match env::var("GOLDEN_DIR") {
        Ok(dir) => (PathBuf::from(dir), "GOLDEN_DIR"),
        Err(_) => (
            Path::new(env!("CARGO_MANIFEST_DIR"))
                .parent()
                .expect("fleet crate is inside the repo")
                .join("src/cmd/gha-runner-tui/testdata/contract"),
            "default golden directory",
        ),
    };
    assert!(
        path.is_dir(),
        "{source} {} does not exist; T1 fixtures are required (set GOLDEN_DIR to src/cmd/gha-runner-tui/testdata/contract)",
        path.display()
    );
    path
}

fn local_fixture(scenario: &str) -> Value {
    let path = common::fixture(&format!("{scenario}.json"));
    serde_json::from_str(&fs::read_to_string(path).expect("local fixture")).expect("fixture JSON")
}

fn sidecar_exit(dir: &Path, scenario: &str) -> Option<i32> {
    let path = dir.join(format!("{scenario}.exit"));
    fs::read_to_string(path)
        .ok()
        .map(|text| text.trim().parse().expect("sidecar exit code"))
}

/// Compare contract shape: object key sets and value types, ignoring sample
/// values, array lengths and formatting. This catches added/renamed/mistyped
/// fields without requiring the two sample datasets to be identical.
fn compare_schema(ours: &Value, theirs: &Value) -> Result<(), String> {
    match (ours, theirs) {
        (Value::Object(ours), Value::Object(theirs)) => {
            let mut ours_keys: Vec<&String> = ours.keys().collect();
            let mut theirs_keys: Vec<&String> = theirs.keys().collect();
            ours_keys.sort();
            theirs_keys.sort();
            if ours_keys != theirs_keys {
                return Err(format!(
                    "object keys differ: local {:?}, golden {:?}",
                    ours_keys, theirs_keys
                ));
            }
            for key in ours_keys {
                compare_schema(&ours[key], &theirs[key])?;
            }
            Ok(())
        }
        (Value::Array(ours), Value::Array(theirs)) => {
            if let (Some(ours_first), Some(theirs_first)) = (ours.first(), theirs.first()) {
                compare_schema(ours_first, theirs_first)?;
            }
            Ok(())
        }
        (Value::Bool(_), Value::Bool(_))
        | (Value::String(_), Value::String(_))
        | (Value::Number(_), Value::Number(_))
        | (Value::Null, Value::Null) => Ok(()),
        _ => Err(format!(
            "value types differ: local {}, golden {}",
            json_kind(ours),
            json_kind(theirs)
        )),
    }
}

fn json_kind(value: &Value) -> &'static str {
    match value {
        Value::Null => "null",
        Value::Bool(_) => "bool",
        Value::Number(_) => "number",
        Value::String(_) => "string",
        Value::Array(_) => "array",
        Value::Object(_) => "object",
    }
}

fn check_expectation(expectation: &Expectation, value: &Value) -> Result<(), String> {
    match expectation.kind {
        Kind::Export => {
            let export: ExportOutput = serde_json::from_value(value.clone())
                .map_err(|e| format!("{}: decode export: {e}", expectation.scenario))?;
            match expectation.scenario {
                "export-complete" => {
                    if !export.complete || !export.errors.is_empty() {
                        return Err(format!(
                            "export-complete must be complete with no errors, got complete={} errors={}",
                            export.complete,
                            export.errors.len()
                        ));
                    }
                    if export
                        .profiles
                        .iter()
                        .any(|profile| profile.owner != "fleet")
                    {
                        return Err(
                            "export was requested with --owner fleet but contains a non-fleet profile"
                                .into(),
                        );
                    }
                }
                "export-incomplete" => {
                    if export.complete || export.errors.is_empty() {
                        return Err(format!(
                            "export-incomplete must be complete=false with errors, got complete={} errors={}",
                            export.complete,
                            export.errors.len()
                        ));
                    }
                }
                other => return Err(format!("unexpected export scenario {other}")),
            }
        }
        Kind::Plan => {
            let plan: PlanOutput = serde_json::from_value(value.clone())
                .map_err(|e| format!("{}: decode plan: {e}", expectation.scenario))?;
            match expectation.scenario {
                "plan-mixed" => {
                    if plan.phase != "plan" || plan.complete {
                        return Err(format!(
                            "plan-mixed must be phase=plan complete=false, got phase={} complete={}",
                            plan.phase, plan.complete
                        ));
                    }
                    let actions: Vec<&str> =
                        plan.results.iter().map(|r| r.action.as_str()).collect();
                    for required in ["create", "update", "unchanged", "restart"] {
                        if !actions.contains(&required) {
                            return Err(format!(
                                "plan-mixed is missing action {required:?}: {actions:?}"
                            ));
                        }
                    }
                    if !plan
                        .results
                        .iter()
                        .any(|r| r.action == "update" && r.restart_needed)
                        || !plan
                            .results
                            .iter()
                            .any(|r| r.action == "update" && !r.restart_needed)
                    {
                        return Err(
                            "plan-mixed must have both restarting and non-restarting updates"
                                .into(),
                        );
                    }
                }
                "plan-conflict" => {
                    if plan.phase != "plan" || plan.complete {
                        return Err(format!(
                            "plan-conflict must be phase=plan complete=false, got phase={} complete={}",
                            plan.phase, plan.complete
                        ));
                    }
                    if !plan.results.iter().any(|r| r.action == "conflict") {
                        return Err("plan-conflict has no conflict result".into());
                    }
                }
                "plan-stale" => {
                    if plan.phase != "plan" || plan.complete {
                        return Err(format!(
                            "plan-stale rejection must be phase=plan complete=false, got phase={} complete={}",
                            plan.phase, plan.complete
                        ));
                    }
                }
                "execute-ok" => {
                    if plan.phase != "execute" || !plan.complete {
                        return Err(format!(
                            "execute-ok must be phase=execute complete=true, got phase={} complete={}",
                            plan.phase, plan.complete
                        ));
                    }
                    if !plan.results.iter().all(|r| r.outcome == "done") {
                        return Err("execute-ok has a non-done outcome".into());
                    }
                }
                "execute-partial" => {
                    if plan.phase != "execute" || plan.complete {
                        return Err(format!(
                            "execute-partial must be phase=execute complete=false, got phase={} complete={}",
                            plan.phase, plan.complete
                        ));
                    }
                    if !plan
                        .results
                        .iter()
                        .any(|r| r.outcome == "failed" || r.restart == "failed")
                        || !plan.results.iter().any(|r| r.outcome == "not_run")
                    {
                        return Err(
                            "execute-partial must have a failed/restart-failed result followed by not_run results"
                                .into(),
                        );
                    }
                }
                "adopt-ok" => {
                    if plan.phase == "plan" {
                        if plan.complete {
                            return Err("adopt-ok dry-run must be complete=false".into());
                        }
                        if !plan.results.iter().any(|r| r.action == "adopt") {
                            return Err("adopt-ok has no adopt action".into());
                        }
                    } else if plan.phase == "execute" {
                        if !plan.complete {
                            return Err("adopt-ok execute must be complete=true".into());
                        }
                        if !plan.results.iter().all(|r| r.outcome == "done") {
                            return Err("adopt-ok has a non-done outcome".into());
                        }
                        if !plan.results.iter().any(|r| r.action == "adopt") {
                            return Err("adopt-ok has no adopt action".into());
                        }
                    } else {
                        return Err(format!("adopt-ok has unexpected phase {:?}", plan.phase));
                    }
                }
                other => return Err(format!("unexpected plan scenario {other}")),
            }
        }
        Kind::Runtime => {
            let runtime: RuntimeOutput = serde_json::from_value(value.clone())
                .map_err(|e| format!("{}: decode runtime: {e}", expectation.scenario))?;
            if runtime.results.is_empty() {
                return Err("match-mode has no results".into());
            }
            if runtime
                .results
                .iter()
                .any(|result| result.outcome != "done")
            {
                return Err("match-mode must have only done outcomes".into());
            }
        }
    }
    Ok(())
}

fn run_center_flow(_dir: &Path, expectation: &Expectation, golden: &Value) {
    let scenario = expectation.scenario;
    match scenario {
        "export-complete" => {
            let env = TestEnv::new(vec![FakeHost::new("s1")]);
            env.write_response("s1", "export", &serde_json::to_string(golden).unwrap())
                .write_exit("s1", "export", expectation.exit_code);
            let output = env.run_yes(&["import", "--host", "s1"], &[]);
            assert_eq!(code(&output), 0, "{scenario}: {}", stderr(&output));
            assert!(env.profiles_dir("s1").exists());
        }
        "export-incomplete" => {
            let env = TestEnv::new(vec![FakeHost::new("s1")]);
            env.write_response("s1", "export", &serde_json::to_string(golden).unwrap())
                .write_exit("s1", "export", expectation.exit_code);
            let output = env.run_yes(&["import", "--host", "s1"], &[]);
            assert_eq!(code(&output), 1, "{scenario}: {}", stderr(&output));
            assert!(!env.profiles_dir("s1").exists());
        }
        "plan-mixed" => {
            let env = TestEnv::new(vec![FakeHost::new("s1")]);
            env.write_profile(
                "s1",
                "a",
                "name: a\nservice:\n  name: a\nmanaged_by: fleet\n",
            )
            .write_response(
                "s1",
                "apply.preview",
                &serde_json::to_string(golden).unwrap(),
            )
            .write_exit("s1", "apply.preview", expectation.exit_code);
            let output = env.run_yes(&["plan", "--host", "s1"], &[]);
            assert_eq!(code(&output), 0, "{scenario}: {}", stderr(&output));
            assert!(stdout(&output).contains("s1: plan"), "{}", stdout(&output));
        }
        "plan-conflict" => {
            let env = TestEnv::new(vec![FakeHost::new("s1")]);
            env.write_profile(
                "s1",
                "a",
                "name: a\nservice:\n  name: a\nmanaged_by: fleet\n",
            )
            .write_response(
                "s1",
                "apply.preview",
                &serde_json::to_string(golden).unwrap(),
            )
            .write_exit("s1", "apply.preview", expectation.exit_code);
            let output = env.run_yes(&["plan", "--host", "s1"], &[]);
            assert_eq!(code(&output), 1, "{scenario}: {}", stderr(&output));
            assert!(stdout(&output).contains("rejected"), "{}", stdout(&output));
        }
        "plan-stale" => {
            let env = TestEnv::new(vec![FakeHost::new("s1")]);
            env.write_profile(
                "s1",
                "a",
                "name: a\nservice:\n  name: a\nmanaged_by: fleet\n",
            )
            .write_response_file("s1", "apply.preview", &common::fixture("plan-mixed.json"))
            .write_response(
                "s1",
                "apply.execute",
                &serde_json::to_string(golden).unwrap(),
            )
            .write_exit("s1", "apply.execute", expectation.exit_code);
            let output = env.run_yes(&["apply", "--yes", "--host", "s1"], &[]);
            assert_eq!(code(&output), 1, "{scenario}: {}", stderr(&output));
            assert!(stdout(&output).contains("rejected"), "{}", stdout(&output));
        }
        "execute-ok" | "execute-partial" => {
            let env = TestEnv::new(vec![FakeHost::new("s1")]);
            env.write_profile(
                "s1",
                "a",
                "name: a\nservice:\n  name: a\nmanaged_by: fleet\n",
            )
            .write_response_file("s1", "apply.preview", &common::fixture("plan-mixed.json"))
            .write_response(
                "s1",
                "apply.execute",
                &serde_json::to_string(golden).unwrap(),
            )
            .write_exit("s1", "apply.execute", expectation.exit_code);
            let output = env.run_yes(&["apply", "--yes", "--host", "s1"], &[]);
            let expected = if expectation.exit_code == 0 { 0 } else { 3 };
            assert_eq!(code(&output), expected, "{scenario}: {}", stderr(&output));
            let text = stdout(&output);
            if expectation.exit_code == 0 {
                assert!(text.contains("applied"), "{text}");
            } else {
                assert!(text.contains("partial"), "{text}");
                assert!(
                    text.contains("may have partially executed") || text.contains("failed"),
                    "{text}"
                );
            }
        }
        "adopt-ok" => {
            let plan: PlanOutput = serde_json::from_value(golden.clone()).unwrap();
            let preview = if plan.phase == "plan" {
                plan.clone()
            } else {
                let mut preview = plan.clone();
                preview.phase = "plan".into();
                preview.complete = false;
                preview.results.iter_mut().for_each(|result| {
                    result.outcome = "planned".into();
                });
                preview
            };
            let execute = if plan.phase == "execute" {
                plan.clone()
            } else {
                let mut execute = plan.clone();
                execute.phase = "execute".into();
                execute.complete = true;
                execute.results.iter_mut().for_each(|result| {
                    result.outcome = "done".into();
                });
                execute
            };
            let env = TestEnv::new(vec![FakeHost::new("s1")]);
            env.write_response(
                "s1",
                "adopt.preview",
                &serde_json::to_string(&preview).unwrap(),
            )
            .write_response(
                "s1",
                "adopt.execute",
                &serde_json::to_string(&execute).unwrap(),
            )
            .write_exit("s1", "adopt.execute", expectation.exit_code);
            let output = env.run_yes(&["adopt", "--host", "s1", "--match", "*", "--yes"], &[]);
            assert_eq!(code(&output), 0, "{scenario}: {}", stderr(&output));
            assert!(stdout(&output).contains("applied"), "{}", stdout(&output));
        }
        "match-mode" => {
            let env = TestEnv::new(vec![FakeHost::new("s1")]);
            env.write_response("s1", "mode", &serde_json::to_string(golden).unwrap())
                .write_exit("s1", "mode", expectation.exit_code);
            let output = env.run_yes(&["mode", "--host", "s1", "--match", "*", "2"], &[]);
            assert_eq!(code(&output), 0, "{scenario}: {}", stderr(&output));
            assert!(stdout(&output).contains("done"), "{}", stdout(&output));
        }
        other => panic!("no center flow for {other}"),
    }
}

#[test]
fn go_goldens_decode_and_meet_contract_expectations() {
    let dir = golden_dir();
    println!(
        "golden_contract: checking {} scenarios in {}",
        EXPECTATIONS.len(),
        dir.display()
    );
    for expectation in EXPECTATIONS {
        let scenario = expectation.scenario;
        let path = dir.join(format!("{scenario}.json"));
        assert!(
            path.is_file(),
            "missing golden {scenario}.json in {} (T1 must publish all contract scenarios)",
            dir.display()
        );
        if let Some(sidecar) = sidecar_exit(&dir, scenario) {
            assert_eq!(
                sidecar, expectation.exit_code,
                "{scenario}: sidecar exit code disagrees with the center's expectation"
            );
        }
        let text = fs::read_to_string(&path).expect("read golden");
        let golden: Value = serde_json::from_str(&text)
            .unwrap_or_else(|e| panic!("{scenario}: golden is not valid JSON: {e}\n{text}"));
        check_expectation(expectation, &golden)
            .unwrap_or_else(|e| panic!("{scenario}: {e}\n{text}"));

        let decoded = match expectation.kind {
            Kind::Export => serde_json::to_value(
                serde_json::from_value::<ExportOutput>(golden.clone()).unwrap(),
            )
            .unwrap(),
            Kind::Plan => {
                serde_json::to_value(serde_json::from_value::<PlanOutput>(golden.clone()).unwrap())
                    .unwrap()
            }
            Kind::Runtime => serde_json::to_value(
                serde_json::from_value::<RuntimeOutput>(golden.clone()).unwrap(),
            )
            .unwrap(),
        };
        let ours = local_fixture(scenario);
        compare_schema(&ours, &decoded)
            .unwrap_or_else(|e| panic!("{scenario}: local fixture and golden schema differ: {e}"));
        run_center_flow(&dir, expectation, &golden);
        println!(
            "golden_contract: {scenario} ok (exit {}, decode/schema/flow verified)",
            expectation.exit_code
        );
    }
}
