mod common;

use common::{host_error, host_reachability, parse_hosts_json, FakeHost, MockApi, TestEnv};
use std::io::Write;
use std::process::Stdio;
use std::time::{Duration, Instant};

fn code(output: &std::process::Output) -> i32 {
    output.status.code().expect("exit code")
}

fn stderr(output: &std::process::Output) -> String {
    String::from_utf8_lossy(&output.stderr).into_owned()
}

fn stdout(output: &std::process::Output) -> String {
    String::from_utf8_lossy(&output.stdout).into_owned()
}

const STATUS_JSON: &str = r#"{
  "slot_holders": [],
  "slot_error": null,
  "profiles": [
    {
      "name": "alpha",
      "admission": {"mode": 2, "delay_seconds": 30},
      "service": {"active": "active", "enabled": true},
      "loop": {"state": "idle", "last_transition_at": "2026-11-27T00:00:00Z", "last_error": null},
      "container": {"name": "gha-alpha", "state": "running"},
      "github": {"state": "skipped", "busy": "skipped"},
      "health": "healthy",
      "errors": [],
      "restart_pending": true
    },
    {
      "name": "beta",
      "admission": {"mode": 1},
      "service": {"active": "inactive", "enabled": false},
      "loop": {"state": "stopped", "last_transition_at": "", "last_error": null},
      "container": {"name": "gha-beta", "state": "none"},
      "github": {"state": "skipped", "busy": "skipped"},
      "health": "none",
      "errors": [],
      "restart_pending": false
    }
  ],
  "profile_errors": []
}"#;

#[test]
fn status_is_concurrent_and_forwards_no_github_json() {
    let env = TestEnv::new(vec![FakeHost::new("s1"), FakeHost::new("s2")]);
    for host in ["s1", "s2"] {
        env.write_sleep(host, "status", 1.5)
            .write_response(host, "status", STATUS_JSON);
    }
    let started = Instant::now();
    let output = env.run_yes(&["status", "--json"], &[]);
    let elapsed = started.elapsed();
    assert_eq!(code(&output), 0, "stderr: {}", stderr(&output));
    assert!(
        elapsed < Duration::from_millis(2800),
        "hosts did not run concurrently: {elapsed:?}"
    );
    let json = parse_hosts_json(&stdout(&output));
    assert_eq!(host_reachability(&json, "s1"), "ok");
    assert_eq!(host_reachability(&json, "s2"), "ok");
    assert_eq!(json["hosts"][0]["status"]["profiles"][0]["name"], "alpha");

    for host in ["s1", "s2"] {
        let log = env.argv_log(host);
        assert!(log.contains("ARG 'status'"), "{log}");
        assert!(log.contains("ARG '--json'"), "{log}");
        assert!(log.contains("ARG '--no-github'"), "{log}");
        assert!(!log.contains("ARG '--github'"), "{log}");
    }
}

#[test]
fn status_text_marks_restart_pending() {
    let env = TestEnv::new(vec![FakeHost::new("s1")]);
    env.write_response("s1", "status", STATUS_JSON);
    let output = env.run_yes(&["status"], &[]);
    assert_eq!(code(&output), 0, "stderr: {}", stderr(&output));
    let text = stdout(&output);
    assert!(
        text.contains("PROFILE\tMODE\tSERVICE\tLOOP\tCONTAINER\tHEALTH"),
        "{text}"
    );
    assert!(
        text.contains("alpha\t2/30s\tactive/enabled\tidle\trunning\thealthy *"),
        "{text}"
    );
    assert!(
        text.contains("beta\t1\tinactive/disabled\tstopped\tnone\tnone"),
        "{text}"
    );
    assert!(text.contains("* restart pending"), "{text}");
}

#[test]
fn apply_preview_and_execute_reuse_the_same_plan_id_and_bundle() {
    let env = TestEnv::new(vec![FakeHost::new("s1")]);
    env.write_profile(
        "s1",
        "a-create",
        "name: a-create\nservice:\n  name: a-create\nmanaged_by: fleet\n",
    )
    .write_profile(
        "s1",
        "b-update-restart",
        "name: b-update-restart\nservice:\n  name: b-update-restart\nmanaged_by: fleet\n",
    )
    .write_response_file("s1", "apply.preview", &common::fixture("plan-mixed.json"))
    .write_response_file("s1", "apply.execute", &common::fixture("execute-ok.json"));

    let output = env.run_yes(&["apply", "--yes", "--host", "s1"], &[]);
    assert_eq!(code(&output), 0, "stderr: {}", stderr(&output));
    let text = stdout(&output);
    assert!(text.contains("s1: plan sha256:0123"), "{text}");
    assert!(text.contains("s1: applied"), "{text}");
    assert!(text.contains("2 update"), "{text}");

    let log = env.argv_log("s1");
    let preview_id = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef";
    assert!(
        log.contains(&format!("ARG '--expect-plan'\nARG '{preview_id}'")),
        "execute must reuse the preview plan_id:\n{log}"
    );
    assert_eq!(log.matches("CALL s1.apply.execute").count(), 1, "{log}");
    assert_eq!(log.matches("CALL s1.apply.preview").count(), 1, "{log}");
    assert!(
        log.contains("ARG --\nARG s1\nARG sudo\nARG -n\nARG '/opt/gha-runner-tui/4469c7b/bin/gha-runner-tui'"),
        "ssh target must follow -- and remote args must be single-quoted:\n{log}"
    );

    let preview_stdin = env.actual_stdin("s1", "apply.preview");
    let execute_stdin = env.actual_stdin("s1", "apply.execute");
    assert_eq!(preview_stdin, execute_stdin);
    assert!(preview_stdin.contains("name: a-create"));
    assert!(preview_stdin.contains("---\nname: b-update-restart"));
}

#[test]
fn plan_stale_is_rejected_without_retry() {
    let env = TestEnv::new(vec![FakeHost::new("s1")]);
    env.write_profile(
        "s1",
        "a-create",
        "name: a-create\nservice:\n  name: a-create\nmanaged_by: fleet\n",
    )
    .write_response_file("s1", "apply.preview", &common::fixture("plan-mixed.json"))
    .write_response_file("s1", "apply.execute", &common::fixture("plan-stale.json"))
    .write_exit("s1", "apply.execute", 1);

    let output = env.run_yes(&["apply", "--yes", "--host", "s1"], &[]);
    assert_eq!(code(&output), 1, "stderr: {}", stderr(&output));
    let text = stdout(&output);
    assert!(text.contains("s1: rejected"), "{text}");
    assert!(
        text.contains("plan") && text.contains("changed") || text.contains("rejected"),
        "{text}"
    );
    let log = env.argv_log("s1");
    assert_eq!(
        log.matches("CALL s1.apply.execute").count(),
        1,
        "must not retry:\n{log}"
    );
}

#[test]
fn adopt_previews_then_executes_with_the_preview_plan_id() {
    let env = TestEnv::new(vec![FakeHost::new("s1")]);
    let preview = r#"{
      "phase": "plan",
      "plan_id": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
      "complete": false,
      "results": [
        {"name": "alpha", "action": "adopt", "restart_needed": false,
         "outcome": "planned", "restart": "none", "message": "unit not installed"}
      ]
    }"#;
    env.write_response("s1", "adopt.preview", preview)
        .write_response_file("s1", "adopt.execute", &common::fixture("adopt-ok.json"));

    let output = env.run_yes(&["adopt", "--host", "s1", "--match", "*", "--yes"], &[]);
    assert_eq!(code(&output), 0, "stderr: {}", stderr(&output));
    let text = stdout(&output);
    assert!(text.contains("s1: plan sha256:aaaa"), "{text}");
    assert!(text.contains("s1: applied"), "{text}");
    assert!(text.contains("1 adopt, 1 unchanged"), "{text}");

    let log = env.argv_log("s1");
    assert!(log.contains("ARG 'adopt'"));
    assert!(log.contains("ARG '--owner'\nARG 'fleet'"), "{log}");
    assert!(log.contains("ARG '--match'\nARG '*'"), "{log}");
    assert!(
        log.contains("ARG '--expect-plan'\nARG 'sha256:aaaa"),
        "{log}"
    );
    assert!(log.contains("ARG '--dry-run'"), "{log}");
}

#[test]
fn mode_forwards_match_and_mode_arguments() {
    let env = TestEnv::new(vec![FakeHost::new("s1")]);
    env.write_response_file("s1", "mode", &common::fixture("match-mode.json"));
    let output = env.run_yes(
        &[
            "mode",
            "--host",
            "s1",
            "--match",
            "*",
            "2",
            "--delay-seconds",
            "30",
        ],
        &[],
    );
    assert_eq!(code(&output), 0, "stderr: {}", stderr(&output));
    let text = stdout(&output);
    assert!(text.contains("s1: done (2 target(s))"), "{text}");
    let log = env.argv_log("s1");
    assert!(log.contains("ARG 'mode'"), "{log}");
    assert!(log.contains("ARG '--match'\nARG '*'"), "{log}");
    assert!(log.contains("ARG '2'"), "{log}");
    assert!(log.contains("ARG '--delay-seconds'\nARG '30'"), "{log}");
}

#[test]
fn timeout_invalid_json_and_ambiguous_disconnect_are_unknown_exit_3() {
    let env = TestEnv::new(vec![
        FakeHost {
            timeout_seconds: 1,
            ..FakeHost::new("s1")
        },
        FakeHost::new("s2"),
        FakeHost::new("s3"),
    ]);
    env.write_timeout_sleep("s1", "status", 5.0)
        .write_response("s2", "status", "not json")
        .write_stderr(
            "s3",
            "status",
            "client_loop: send disconnect: Broken pipe\n",
        )
        .write_exit("s3", "status", 255);

    let output = env.run_yes(&["status", "--json"], &[]);
    assert_eq!(code(&output), 3, "stderr: {}", stderr(&output));
    let json = parse_hosts_json(&stdout(&output));
    assert_eq!(host_reachability(&json, "s1"), "unknown");
    assert_eq!(host_reachability(&json, "s2"), "unknown");
    assert_eq!(host_reachability(&json, "s3"), "unknown");
    let s1_error = host_error(&json, "s1").unwrap();
    assert!(s1_error.contains("timed out"), "{s1_error}");
    let s3_error = host_error(&json, "s3").unwrap();
    assert!(s3_error.contains("255"), "{s3_error}");
}

#[test]
fn refused_connection_255_stays_unknown() {
    let env = TestEnv::new(vec![FakeHost::new("s1")]);
    env.write_stderr(
        "s1",
        "status",
        "ssh: connect to host s1 port 22: Connection refused\n",
    )
    .write_exit("s1", "status", 255);
    let output = env.run_yes(&["status", "--json"], &[]);
    assert_eq!(code(&output), 3, "stderr: {}", stderr(&output));
    let json = parse_hosts_json(&stdout(&output));
    assert_eq!(host_reachability(&json, "s1"), "unknown");
    let error = host_error(&json, "s1").unwrap();
    assert!(error.contains("255"), "{error}");
}

#[test]
fn apply_unknown_suggests_plan_follow_up() {
    let env = TestEnv::new(vec![FakeHost::new("s1")]);
    env.write_profile(
        "s1",
        "a-create",
        "name: a-create\nservice:\n  name: a-create\nmanaged_by: fleet\n",
    )
    .write_response_file("s1", "apply.preview", &common::fixture("plan-mixed.json"))
    .write_stderr(
        "s1",
        "apply.execute",
        "client_loop: send disconnect: Broken pipe\n",
    )
    .write_exit("s1", "apply.execute", 255);
    let output = env.run_yes(&["apply", "--yes", "--host", "s1"], &[]);
    assert_eq!(code(&output), 3, "stderr: {}", stderr(&output));
    let text = stdout(&output);
    assert!(text.contains("may have partially executed"), "{text}");
    assert!(text.contains("gha-fleet plan --host"), "{text}");
    assert_eq!(
        env.argv_log("s1").matches("CALL s1.apply.execute").count(),
        1,
        "unknown must not be retried"
    );
}

#[test]
fn non_tty_without_yes_exits_2_before_execute() {
    let env = TestEnv::new(vec![FakeHost::new("s1")]);
    env.write_profile(
        "s1",
        "a-create",
        "name: a-create\nservice:\n  name: a-create\nmanaged_by: fleet\n",
    )
    .write_response_file("s1", "apply.preview", &common::fixture("plan-mixed.json"));
    let output = env.run(&["apply", "--host", "s1"], &[], Stdio::null());
    assert_eq!(code(&output), 2, "stderr: {}", stderr(&output));
    let log = env.argv_log("s1");
    assert!(!log.contains("CALL s1.apply.execute"), "{log}");
}

#[test]
fn empty_expected_state_with_prune_is_rejected_without_ssh() {
    let env = TestEnv::new(vec![FakeHost::new("s1")]);
    let output = env.run_yes(&["plan", "--prune", "--host", "s1"], &[]);
    assert_eq!(code(&output), 1, "stderr: {}", stderr(&output));
    assert!(
        stderr(&output).contains("refusing --prune"),
        "{}",
        stderr(&output)
    );
    assert!(env.argv_log("s1").is_empty());
}

#[test]
fn incomplete_export_import_writes_nothing() {
    let env = TestEnv::new(vec![FakeHost::new("s1")]);
    env.write_response_file("s1", "export", &common::fixture("export-incomplete.json"))
        .write_exit("s1", "export", 1);
    let output = env.run_yes(&["import", "--host", "s1"], &[]);
    assert_eq!(code(&output), 1, "stderr: {}", stderr(&output));
    assert!(!env.profiles_dir("s1").exists());
    let text = stdout(&output);
    assert!(text.contains("export incomplete"), "{text}");
}

#[test]
fn import_writes_exact_files_and_refuses_divergence_until_forced() {
    let env = TestEnv::new(vec![FakeHost::new("s1")]);
    env.write_response_file("s1", "export", &common::fixture("export-complete.json"));

    let first = env.run_yes(&["import", "--host", "s1"], &[]);
    assert_eq!(code(&first), 0, "stderr: {}", stderr(&first));
    assert_eq!(
        std::fs::read_to_string(env.profiles_dir("s1").join("alpha.yaml")).unwrap(),
        "name: alpha\nservice:\n  name: alpha\nmanaged_by: fleet\n"
    );

    let second = env.run_yes(&["import", "--host", "s1"], &[]);
    assert_eq!(code(&second), 0, "stderr: {}", stderr(&second));
    assert!(
        stdout(&second).contains("2 unchanged"),
        "{}",
        stdout(&second)
    );

    std::fs::write(
        env.profiles_dir("s1").join("alpha.yaml"),
        "name: alpha\nmanaged_by: fleet\n# edited locally\n",
    )
    .unwrap();
    let conflict = env.run_yes(&["import", "--host", "s1"], &[]);
    assert_eq!(code(&conflict), 1, "stderr: {}", stderr(&conflict));
    assert!(
        stdout(&conflict).contains("refusing import"),
        "{}",
        stdout(&conflict)
    );
    assert_eq!(
        std::fs::read_to_string(env.profiles_dir("s1").join("alpha.yaml")).unwrap(),
        "name: alpha\nmanaged_by: fleet\n# edited locally\n"
    );

    let forced = env.run_yes(&["import", "--host", "s1", "--force"], &[]);
    assert_eq!(code(&forced), 0, "stderr: {}", stderr(&forced));
    assert!(
        stdout(&forced).contains("1 overwritten, 1 unchanged"),
        "{}",
        stdout(&forced)
    );

    std::fs::write(env.profiles_dir("s1").join("local-only.yaml"), "x").unwrap();
    let reported = env.run_yes(&["import", "--host", "s1"], &[]);
    assert_eq!(code(&reported), 0, "stderr: {}", stderr(&reported));
    assert!(
        stdout(&reported).contains("local-only"),
        "{}",
        stdout(&reported)
    );
    assert!(env.profiles_dir("s1").join("local-only.yaml").exists());
}

#[test]
fn asleep_host_is_skipped_before_ssh_for_status_and_apply() {
    let api = MockApi::start(r#"{"state":"sleeping","desiredState":"sleeping"}"#);
    let env = TestEnv::new(vec![FakeHost::gated("s1", "m_s1", "S1_KEY")]);
    env.write_profile(
        "s1",
        "a-create",
        "name: a-create\nservice:\n  name: a-create\nmanaged_by: fleet\n",
    );
    let envs = [
        ("S1_KEY", "secret"),
        ("GHA_FLEET_INSTABOX_API_BASE", api.base.as_str()),
    ];

    let status = env.run_yes(&["status", "--json"], &envs);
    assert_eq!(code(&status), 0, "stderr: {}", stderr(&status));
    let json = parse_hosts_json(&stdout(&status));
    assert_eq!(host_reachability(&json, "s1"), "asleep");
    assert!(
        env.argv_log("s1").is_empty(),
        "asleep host must not be SSH'd"
    );
    assert_eq!(api.requests().len(), 1);

    let apply = env.run_yes(&["apply", "--yes", "--host", "s1"], &envs);
    assert_eq!(code(&apply), 1, "stderr: {}", stderr(&apply));
    let text = stdout(&apply);
    assert!(text.contains("asleep"), "{text}");
    assert!(env.argv_log("s1").is_empty());
}

#[test]
fn gate_requires_api_key_and_running_state() {
    let api = MockApi::start(r#"{"state":"running"}"#);
    let env = TestEnv::new(vec![FakeHost::gated("s1", "m_s1", "S1_KEY")]);
    env.write_response_file("s1", "status", &common::fixture("export-complete.json"));

    let missing_key = env.run_yes(
        &["status", "--host", "s1", "--json"],
        &[("GHA_FLEET_INSTABOX_API_BASE", api.base.as_str())],
    );
    assert_eq!(code(&missing_key), 1, "stderr: {}", stderr(&missing_key));
    let json = parse_hosts_json(&stdout(&missing_key));
    assert_eq!(host_reachability(&json, "s1"), "error");
    assert!(host_error(&json, "s1")
        .unwrap()
        .contains("missing or empty"));
    assert!(env.argv_log("s1").is_empty());

    env.write_response("s1", "status", STATUS_JSON);
    let running = env.run_yes(
        &["status", "--host", "s1", "--json"],
        &[
            ("S1_KEY", "secret"),
            ("GHA_FLEET_INSTABOX_API_BASE", api.base.as_str()),
        ],
    );
    assert_eq!(code(&running), 0, "stderr: {}", stderr(&running));
    let json = parse_hosts_json(&stdout(&running));
    assert_eq!(host_reachability(&json, "s1"), "ok");
    assert!(
        common::wait_for(Duration::from_secs(2), || {
            api.requests()
                .iter()
                .any(|line| line.contains("GET /v1/machines/m_s1 HTTP/1.1"))
        }),
        "requests: {:?}",
        api.requests()
    );
    assert!(env.argv_log("s1").contains("CALL s1.status"));
}

#[test]
fn status_cached_reports_age_and_staleness_without_ssh() {
    let env = TestEnv::new(vec![FakeHost::new("s1"), FakeHost::new("s2")]);
    let snapshots = env.state.join("snapshots");
    std::fs::create_dir_all(&snapshots).unwrap();
    let snapshot = format!(
        r#"{{
          "last_ok": {{"observed_at": "2020-01-01T00:00:00Z", "status": {}}},
          "last_attempt": {{"at": "2020-01-01T00:00:00Z", "reachability": "ok", "error": null}}
        }}"#,
        STATUS_JSON
    );
    std::fs::write(snapshots.join("s1.json"), snapshot).unwrap();

    let output = env.run_yes(&["status", "--cached"], &[]);
    assert_eq!(code(&output), 0, "stderr: {}", stderr(&output));
    let text = stdout(&output);
    assert!(text.contains("s1: ok"), "{text}");
    assert!(
        text.contains("last ok") && text.contains("(stale)"),
        "{text}"
    );
    assert!(
        text.contains("s2") && text.contains("no snapshot"),
        "{text}"
    );
    assert!(env.argv_log("s1").is_empty());
}

#[test]
fn status_node_failure_with_valid_json_is_error_exit_1() {
    let env = TestEnv::new(vec![FakeHost::new("s1")]);
    env.write_response("s1", "status", "{}")
        .write_exit("s1", "status", 7)
        .write_stderr("s1", "status", "unexpected failure\n");
    let output = env.run_yes(&["status", "--host", "s1"], &[]);
    assert_eq!(code(&output), 1, "stderr: {}", stderr(&output));
    let log = env.argv_log("s1");
    assert_eq!(log.matches("CALL s1.status").count(), 1, "{log}");
}

#[test]
fn import_preflight_never_writes_when_one_host_is_uncertain() {
    let env = TestEnv::new(vec![FakeHost::new("s1"), FakeHost::new("s2")]);
    env.write_response_file("s1", "export", &common::fixture("export-complete.json"))
        .write_response("s2", "export", "not json");
    let output = env.run_yes(&["import"], &[]);
    assert_eq!(code(&output), 3, "stderr: {}", stderr(&output));
    assert!(!env.profiles_dir("s1").exists());
    assert!(!env.profiles_dir("s2").exists());
}

#[test]
fn stdin_is_closed_for_commands_without_a_bundle() {
    let env = TestEnv::new(vec![FakeHost::new("s1")]);
    env.write_response("s1", "status", STATUS_JSON);
    let mut child = std::process::Command::new(env!("CARGO_BIN_EXE_gha-fleet"))
        .arg("--dir")
        .arg(&env.expected)
        .args(["status", "--host", "s1"])
        .env(
            "PATH",
            format!("{}:{}", env.fake.display(), std::env::var("PATH").unwrap()),
        )
        .env("FAKE_SSH_DIR", env.root.join("responses"))
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .spawn()
        .unwrap();
    child.stdin.take().unwrap().write_all(b"ignored\n").unwrap();
    let output = child.wait_with_output().unwrap();
    assert_eq!(code(&output), 0, "stderr: {}", stderr(&output));
    assert!(env.stdin_exists("s1", "status"));
    assert_eq!(env.actual_stdin("s1", "status"), "");
}

#[test]
fn apply_partial_execution_exits_3_and_shows_not_run() {
    let env = TestEnv::new(vec![FakeHost::new("s1")]);
    env.write_profile(
        "s1",
        "a-create",
        "name: a-create\nservice:\n  name: a-create\nmanaged_by: fleet\n",
    )
    .write_response_file("s1", "apply.preview", &common::fixture("plan-mixed.json"))
    .write_response_file(
        "s1",
        "apply.execute",
        &common::fixture("execute-partial.json"),
    )
    .write_exit("s1", "apply.execute", 3);
    let output = env.run_yes(&["apply", "--yes", "--host", "s1"], &[]);
    assert_eq!(code(&output), 3, "stderr: {}", stderr(&output));
    let text = stdout(&output);
    assert!(text.contains("s1: partial"), "{text}");
    assert!(text.contains("not_run"), "{text}");
    assert!(
        text.contains("systemctl restart --no-block failed"),
        "{text}"
    );
}

#[test]
fn plan_conflict_is_rejected_exit_1() {
    let env = TestEnv::new(vec![FakeHost::new("s1")]);
    env.write_profile(
        "s1",
        "a",
        "name: a\nservice:\n  name: a\nmanaged_by: fleet\n",
    )
    .write_response_file(
        "s1",
        "apply.preview",
        &common::fixture("plan-conflict.json"),
    )
    .write_exit("s1", "apply.preview", 1);
    let output = env.run_yes(&["plan", "--host", "s1"], &[]);
    assert_eq!(code(&output), 1, "stderr: {}", stderr(&output));
    assert!(
        stdout(&output).contains("s1: rejected"),
        "{}",
        stdout(&output)
    );
}

#[test]
fn ambiguous_255_without_evidence_is_unknown_not_unreachable() {
    let env = TestEnv::new(vec![FakeHost::new("s1")]);
    env.write_exit("s1", "status", 255);
    let output = env.run_yes(&["status", "--host", "s1", "--json"], &[]);
    assert_eq!(code(&output), 3, "stderr: {}", stderr(&output));
    let json = parse_hosts_json(&stdout(&output));
    assert_eq!(host_reachability(&json, "s1"), "unknown");
}

#[test]
fn apply_does_not_report_unknown_hosts_as_applied() {
    let env = TestEnv::new(vec![FakeHost::new("s1"), FakeHost::new("s2")]);
    env.write_profile(
        "s1",
        "a-create",
        "name: a-create\nservice:\n  name: a-create\nmanaged_by: fleet\n",
    )
    .write_profile(
        "s2",
        "b-create",
        "name: b-create\nservice:\n  name: b-create\nmanaged_by: fleet\n",
    )
    .write_response_file("s1", "apply.preview", &common::fixture("plan-mixed.json"))
    .write_response_file("s1", "apply.execute", &common::fixture("execute-ok.json"))
    .write_stderr(
        "s2",
        "apply.preview",
        "ssh: connect to host s2 port 22: Connection refused\n",
    )
    .write_exit("s2", "apply.preview", 255);
    let output = env.run_yes(&["apply", "--yes"], &[]);
    assert_eq!(code(&output), 3, "stderr: {}", stderr(&output));
    let text = stdout(&output);
    assert!(text.contains("s1: applied"), "{text}");
    assert!(text.contains("s2: unknown"), "{text}");
    assert!(!text.contains("s2: applied"), "{text}");
}

#[test]
fn runtime_attempted_failure_is_partial_exit_3() {
    let env = TestEnv::new(vec![FakeHost::new("s1")]);
    let response = r#"{
      "results": [
        {"name": "alpha", "outcome": "done", "message": ""},
        {"name": "beta", "outcome": "failed", "message": "systemctl restart: unit not found"}
      ]
    }"#;
    env.write_response("s1", "restart", response)
        .write_exit("s1", "restart", 3);
    let output = env.run_yes(&["restart", "--host", "s1", "--match", "*"], &[]);
    assert_eq!(code(&output), 3, "stderr: {}", stderr(&output));
    let text = stdout(&output);
    assert!(text.contains("s1: partial"), "{text}");
    assert!(
        text.contains("beta: failed: systemctl restart: unit not found"),
        "{text}"
    );
}

#[test]
fn runtime_preflight_refusal_is_failed_exit_1() {
    let env = TestEnv::new(vec![FakeHost::new("s1")]);
    env.write_response("s1", "start", r#"{"results": []}"#)
        .write_stderr("s1", "start", "no profiles matched\n")
        .write_exit("s1", "start", 1);
    let output = env.run_yes(&["start", "--host", "s1", "--match", "nope-*"], &[]);
    assert_eq!(code(&output), 1, "stderr: {}", stderr(&output));
    assert!(
        stdout(&output).contains("s1: failed"),
        "{}",
        stdout(&output)
    );
}

#[test]
fn adopt_with_only_unchanged_profiles_makes_no_write() {
    let env = TestEnv::new(vec![FakeHost::new("s1")]);
    let preview = r#"{
      "phase": "plan",
      "plan_id": "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
      "complete": false,
      "results": [
        {"name": "alpha", "action": "unchanged", "restart_needed": false,
         "outcome": "planned", "restart": "none", "message": ""}
      ]
    }"#;
    env.write_response("s1", "adopt.preview", preview);
    let output = env.run_yes(&["adopt", "--host", "s1", "--match", "*"], &[]);
    assert_eq!(code(&output), 0, "stderr: {}", stderr(&output));
    assert!(
        stdout(&output).contains("no ownership changes"),
        "{}",
        stdout(&output)
    );
    assert!(!env.argv_log("s1").contains("CALL s1.adopt.execute"));
}

#[test]
fn fleet_dir_environment_variable_is_the_default_directory() {
    let env = TestEnv::new(vec![FakeHost::new("s1")]);
    env.write_response("s1", "status", STATUS_JSON);
    let output = std::process::Command::new(env!("CARGO_BIN_EXE_gha-fleet"))
        .args(["status", "--host", "s1", "--json"])
        .env("GHA_FLEET_DIR", &env.expected)
        .env(
            "PATH",
            format!("{}:{}", env.fake.display(), std::env::var("PATH").unwrap()),
        )
        .env("FAKE_SSH_DIR", env.root.join("responses"))
        .stdin(Stdio::null())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .output()
        .unwrap();
    assert_eq!(code(&output), 0, "stderr: {}", stderr(&output));
    let json = parse_hosts_json(&stdout(&output));
    assert_eq!(host_reachability(&json, "s1"), "ok");
}

#[test]
fn status_cached_json_forwards_snapshot_status() {
    let env = TestEnv::new(vec![FakeHost::new("s1")]);
    let snapshots = env.state.join("snapshots");
    std::fs::create_dir_all(&snapshots).unwrap();
    let snapshot = format!(
        r#"{{
          "last_ok": {{"observed_at": "2026-11-27T00:00:00Z", "status": {}}},
          "last_attempt": {{"at": "2026-11-27T00:00:00Z", "reachability": "ok", "error": null}}
        }}"#,
        STATUS_JSON
    );
    std::fs::write(snapshots.join("s1.json"), snapshot).unwrap();
    let output = env.run_yes(&["status", "--cached", "--json"], &[]);
    assert_eq!(code(&output), 0, "stderr: {}", stderr(&output));
    let json = parse_hosts_json(&stdout(&output));
    assert_eq!(host_reachability(&json, "s1"), "ok");
    assert_eq!(json["hosts"][0]["status"]["profiles"][0]["name"], "alpha");
    assert!(env.argv_log("s1").is_empty());
}

#[test]
fn apply_prune_is_passed_to_both_preview_and_execute() {
    let env = TestEnv::new(vec![FakeHost::new("s1")]);
    env.write_profile(
        "s1",
        "a",
        "name: a\nservice:\n  name: a\nmanaged_by: fleet\n",
    )
    .write_response_file("s1", "apply.preview", &common::fixture("plan-mixed.json"))
    .write_response_file("s1", "apply.execute", &common::fixture("execute-ok.json"));
    let output = env.run_yes(&["apply", "--yes", "--prune", "--host", "s1"], &[]);
    assert_eq!(code(&output), 0, "stderr: {}", stderr(&output));
    let log = env.argv_log("s1");
    assert_eq!(log.matches("ARG '--prune'").count(), 2, "{log}");
    assert!(log.contains("CALL s1.apply.preview"), "{log}");
    assert!(log.contains("CALL s1.apply.execute"), "{log}");
}

#[test]
fn adopt_owner_local_is_forwarded_to_preview_and_execute() {
    let env = TestEnv::new(vec![FakeHost::new("s1")]);
    let preview = r#"{
      "phase": "plan",
      "plan_id": "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
      "complete": false,
      "results": [
        {"name": "alpha", "action": "adopt", "restart_needed": false,
         "outcome": "planned", "restart": "none", "message": ""}
      ]
    }"#;
    let execute = r#"{
      "phase": "execute",
      "plan_id": "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
      "complete": true,
      "results": [
        {"name": "alpha", "action": "adopt", "restart_needed": false,
         "outcome": "done", "restart": "none", "message": ""}
      ]
    }"#;
    env.write_response("s1", "adopt.preview", preview)
        .write_response("s1", "adopt.execute", execute);
    let output = env.run_yes(
        &[
            "adopt", "--host", "s1", "--match", "*", "--owner", "local", "--yes",
        ],
        &[],
    );
    assert_eq!(code(&output), 0, "stderr: {}", stderr(&output));
    let log = env.argv_log("s1");
    assert_eq!(log.matches("ARG 'local'").count(), 2, "{log}");
    assert!(!log.contains("ARG 'fleet'"), "{log}");
}

#[test]
fn argument_errors_exit_2_and_never_call_ssh() {
    let env = TestEnv::new(vec![FakeHost::new("s1")]);
    for args in [
        vec!["mode", "--match", "*"],
        vec!["mode", "--match", "a b", "1"],
        vec!["mode", "--match", "*", "1", "--delay-seconds", "30"],
        vec!["adopt", "--host", "s1"],
        vec!["status", "--prune"],
        vec!["apply", "--json"],
        vec!["start", "--host", "s1"],
        vec!["unknown-command"],
    ] {
        let output = env.run_yes(&args, &[]);
        assert_eq!(code(&output), 2, "args {args:?}: {}", stderr(&output));
    }
    assert!(env.argv_log("s1").is_empty());
}

#[test]
fn status_cached_unknown_snapshot_exits_3_but_missing_snapshot_exits_0() {
    let env = TestEnv::new(vec![FakeHost::new("s1"), FakeHost::new("s2")]);
    let snapshots = env.state.join("snapshots");
    std::fs::create_dir_all(&snapshots).unwrap();
    std::fs::write(
        snapshots.join("s1.json"),
        r#"{
          "last_ok": null,
          "last_attempt": {"at": "2026-11-27T00:00:00Z", "reachability": "unknown", "error": "exec timed out"}
        }"#,
    )
    .unwrap();
    let output = env.run_yes(&["status", "--cached", "--json"], &[]);
    assert_eq!(code(&output), 3, "stderr: {}", stderr(&output));
    let json = parse_hosts_json(&stdout(&output));
    assert_eq!(host_reachability(&json, "s1"), "unknown");
    assert_eq!(host_reachability(&json, "s2"), "unknown");
}

#[test]
fn valid_status_json_with_255_is_still_unknown() {
    let env = TestEnv::new(vec![FakeHost::new("s1")]);
    env.write_response("s1", "status", STATUS_JSON)
        .write_exit("s1", "status", 255);
    let output = env.run_yes(&["status", "--host", "s1", "--json"], &[]);
    assert_eq!(code(&output), 3, "stderr: {}", stderr(&output));
    let json = parse_hosts_json(&stdout(&output));
    assert_eq!(host_reachability(&json, "s1"), "unknown");
    let error = host_error(&json, "s1").unwrap();
    assert!(error.contains("255"), "{error}");
}

#[test]
fn valid_preview_json_with_255_is_unknown_and_executes_nothing() {
    let env = TestEnv::new(vec![FakeHost::new("s1")]);
    env.write_profile(
        "s1",
        "a-create",
        "name: a-create\nservice:\n  name: a-create\nmanaged_by: fleet\n",
    )
    .write_response_file("s1", "apply.preview", &common::fixture("plan-mixed.json"))
    .write_exit("s1", "apply.preview", 255);
    let output = env.run_yes(&["apply", "--yes", "--host", "s1"], &[]);
    assert_eq!(code(&output), 3, "stderr: {}", stderr(&output));
    assert!(
        stdout(&output).contains("s1: unknown"),
        "{}",
        stdout(&output)
    );
    assert!(!env.argv_log("s1").contains("CALL s1.apply.execute"));
}

#[test]
fn valid_execute_json_with_255_is_unknown_and_not_applied() {
    let env = TestEnv::new(vec![FakeHost::new("s1")]);
    env.write_profile(
        "s1",
        "a-create",
        "name: a-create\nservice:\n  name: a-create\nmanaged_by: fleet\n",
    )
    .write_response_file("s1", "apply.preview", &common::fixture("plan-mixed.json"))
    .write_response_file("s1", "apply.execute", &common::fixture("execute-ok.json"))
    .write_exit("s1", "apply.execute", 255);
    let output = env.run_yes(&["apply", "--yes", "--host", "s1"], &[]);
    assert_eq!(code(&output), 3, "stderr: {}", stderr(&output));
    let text = stdout(&output);
    assert!(text.contains("s1: unknown"), "{text}");
    assert!(text.contains("may have partially executed"), "{text}");
    assert!(!text.contains("s1: applied"), "{text}");
    assert_eq!(
        env.argv_log("s1").matches("CALL s1.apply.execute").count(),
        1,
        "unknown must not be retried"
    );
}

#[test]
fn valid_export_json_with_255_is_unknown_and_writes_nothing() {
    let env = TestEnv::new(vec![FakeHost::new("s1")]);
    env.write_response_file("s1", "export", &common::fixture("export-complete.json"))
        .write_exit("s1", "export", 255);
    let output = env.run_yes(&["import", "--host", "s1"], &[]);
    assert_eq!(code(&output), 3, "stderr: {}", stderr(&output));
    assert!(stdout(&output).contains("unknown"), "{}", stdout(&output));
    assert!(!env.profiles_dir("s1").exists());
}

#[test]
fn locally_unspawnable_ssh_is_unreachable_not_unknown() {
    let env = TestEnv::new(vec![FakeHost::new("s1")]);
    let empty = env.root().join("empty-bin");
    std::fs::create_dir_all(&empty).unwrap();
    let path = empty.to_string_lossy().into_owned();
    let output = env.run_yes(
        &["status", "--host", "s1", "--json"],
        &[("PATH", path.as_str())],
    );
    assert_eq!(code(&output), 1, "stderr: {}", stderr(&output));
    let json = parse_hosts_json(&stdout(&output));
    assert_eq!(host_reachability(&json, "s1"), "unreachable");
    let error = host_error(&json, "s1").unwrap();
    assert!(error.contains("could not be started"), "{error}");
}

#[test]
fn descendant_holding_pipes_returns_at_the_deadline_as_unknown() {
    let env = TestEnv::new(vec![FakeHost {
        timeout_seconds: 1,
        ..FakeHost::new("s1")
    }]);
    env.write_response("s1", "status", STATUS_JSON)
        .write_hold_pipe("s1", "status", 6.0);
    let started = Instant::now();
    let output = env.run_yes(&["status", "--host", "s1", "--json"], &[]);
    let elapsed = started.elapsed();
    assert!(
        elapsed < Duration::from_secs(4),
        "command waited for the descendant pipe: {elapsed:?}"
    );
    assert_eq!(code(&output), 3, "stderr: {}", stderr(&output));
    let json = parse_hosts_json(&stdout(&output));
    assert_eq!(host_reachability(&json, "s1"), "unknown");
    let error = host_error(&json, "s1").unwrap();
    assert!(error.contains("timed out"), "{error}");
}

#[cfg(unix)]
#[test]
fn prune_refuses_mixed_regular_and_symlinked_profiles_before_ssh() {
    let env = TestEnv::new(vec![FakeHost::new("s1")]);
    env.write_profile("s1", "a", "name: a\nmanaged_by: fleet\n");
    let dir = env.profiles_dir("s1");
    let target = env.root().join("linked-profile.yaml");
    std::fs::write(&target, "name: linked\nmanaged_by: fleet\n").unwrap();
    std::os::unix::fs::symlink(&target, dir.join("b.yaml")).unwrap();
    let output = env.run_yes(&["plan", "--prune", "--host", "s1"], &[]);
    assert_eq!(code(&output), 1, "stderr: {}", stderr(&output));
    assert!(
        stderr(&output).contains("not a regular file"),
        "{}",
        stderr(&output)
    );
    assert!(env.argv_log("s1").is_empty(), "must refuse before SSH");
}

#[test]
fn import_refuses_case_colliding_existing_file_without_mutation() {
    let env = TestEnv::new(vec![FakeHost::new("s1")]);
    env.write_response_file("s1", "export", &common::fixture("export-complete.json"));
    let dir = env.profiles_dir("s1");
    std::fs::create_dir_all(&dir).unwrap();
    std::fs::write(dir.join("ALPHA.yaml"), "keep\n").unwrap();

    let output = env.run_yes(&["import", "--host", "s1"], &[]);
    assert_eq!(code(&output), 1, "stderr: {}", stderr(&output));
    assert!(
        stdout(&output).contains("refusing import"),
        "{}",
        stdout(&output)
    );
    assert_eq!(
        std::fs::read_to_string(dir.join("ALPHA.yaml")).unwrap(),
        "keep\n"
    );
    assert!(!dir.join("beta.yaml").exists());
    let names: Vec<String> = std::fs::read_dir(&dir)
        .unwrap()
        .map(|entry| entry.unwrap().file_name().to_string_lossy().into_owned())
        .collect();
    assert_eq!(names, vec!["ALPHA.yaml".to_string()]);
}

#[cfg(unix)]
#[test]
fn import_refuses_symlinked_staging_path_without_mutation() {
    let env = TestEnv::new(vec![FakeHost::new("s1")]);
    env.write_response_file("s1", "export", &common::fixture("export-complete.json"));
    let dir = env.profiles_dir("s1");
    std::fs::create_dir_all(&dir).unwrap();
    let victim = env.root().join("staging-victim.txt");
    std::fs::write(&victim, "do not touch\n").unwrap();
    let staging = dir.join(".alpha.yaml.gha-fleet-tmp");
    std::os::unix::fs::symlink(&victim, &staging).unwrap();

    let output = env.run_yes(&["import", "--host", "s1"], &[]);
    assert_eq!(code(&output), 1, "stderr: {}", stderr(&output));
    assert!(
        stdout(&output).contains("import failed"),
        "{}",
        stdout(&output)
    );
    assert_eq!(std::fs::read_to_string(&victim).unwrap(), "do not touch\n");
    assert!(std::fs::symlink_metadata(&staging)
        .unwrap()
        .file_type()
        .is_symlink());
    assert!(!dir.join("alpha.yaml").exists());
    assert!(!dir.join("beta.yaml").exists());
}

#[test]
fn gate_refuses_redirect_without_forwarding_the_key() {
    let target = MockApi::start(r#"{"state":"running"}"#);
    let redirect = MockApi::start_redirect(&format!("{}/v1/machines/m_s1", target.base));
    let env = TestEnv::new(vec![FakeHost::gated("s1", "m_s1", "S1_KEY")]);
    let envs = [
        ("S1_KEY", "dummy-key"),
        ("GHA_FLEET_INSTABOX_API_BASE", redirect.base.as_str()),
    ];
    let output = env.run_yes(&["status", "--host", "s1", "--json"], &envs);
    assert_eq!(code(&output), 1, "stderr: {}", stderr(&output));
    let json = parse_hosts_json(&stdout(&output));
    assert_eq!(host_reachability(&json, "s1"), "error");
    let error = host_error(&json, "s1").unwrap();
    assert!(error.contains("HTTP 302"), "{error}");
    assert_eq!(redirect.requests().len(), 1);
    assert!(
        target.requests().is_empty(),
        "redirect target must never be contacted: {:?}",
        target.requests()
    );
    assert!(env.argv_log("s1").is_empty());
}

#[test]
fn gate_rejects_non_2xx_before_reading_the_body() {
    let api = MockApi::start_status("500 Internal Server Error", r#"{"state":"running"}"#);
    let env = TestEnv::new(vec![FakeHost::gated("s1", "m_s1", "S1_KEY")]);
    let envs = [
        ("S1_KEY", "dummy-key"),
        ("GHA_FLEET_INSTABOX_API_BASE", api.base.as_str()),
    ];
    let output = env.run_yes(&["status", "--host", "s1", "--json"], &envs);
    assert_eq!(code(&output), 1, "stderr: {}", stderr(&output));
    let json = parse_hosts_json(&stdout(&output));
    assert_eq!(host_reachability(&json, "s1"), "error");
    let error = host_error(&json, "s1").unwrap();
    assert!(error.contains("HTTP 500"), "{error}");
    assert_eq!(api.requests().len(), 1);
    assert!(env.argv_log("s1").is_empty());
}
