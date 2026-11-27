//! Shared fake-ssh harness for integration tests.
//!
//! Each test gets a temp directory containing a `fleet.toml`, an expected-state
//! tree, and a `ssh` shim in PATH. The shim records argv and stdin and serves
//! canned contract replies keyed by `<host>.<command>` (apply/adopt add
//! `.preview` or `.execute`).

#![allow(dead_code)]

use std::collections::BTreeMap;
use std::io::{Read, Write};
use std::net::{TcpListener, TcpStream};
use std::path::{Path, PathBuf};
use std::process::{Command, Output, Stdio};
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};
use std::{env, fs, thread};

static COUNTER: AtomicU64 = AtomicU64::new(0);

pub struct TestEnv {
    pub root: PathBuf,
    pub expected: PathBuf,
    pub fake: PathBuf,
    pub state: PathBuf,
    hosts: Vec<FakeHost>,
}

#[derive(Debug, Clone)]
pub struct FakeHost {
    pub name: String,
    pub ssh: String,
    pub machine_id: Option<String>,
    pub api_key_env: Option<String>,
    pub timeout_seconds: u64,
}

impl FakeHost {
    pub fn new(name: &str) -> Self {
        FakeHost {
            name: name.to_string(),
            ssh: name.to_string(),
            machine_id: None,
            api_key_env: None,
            timeout_seconds: 5,
        }
    }

    pub fn gated(name: &str, machine_id: &str, key_env: &str) -> Self {
        FakeHost {
            name: name.to_string(),
            ssh: name.to_string(),
            machine_id: Some(machine_id.to_string()),
            api_key_env: Some(key_env.to_string()),
            timeout_seconds: 5,
        }
    }
}

impl TestEnv {
    pub fn new(hosts: Vec<FakeHost>) -> Self {
        let root = temp_root("gha-fleet-int");
        let expected = root.join("expected");
        let fake = root.join("fakebin");
        let state = root.join("state");
        fs::create_dir_all(&expected).unwrap();
        fs::create_dir_all(&fake).unwrap();
        fs::create_dir_all(&state).unwrap();
        let env = TestEnv {
            root,
            expected,
            fake,
            state,
            hosts,
        };
        env.write_ssh_shim();
        env.write_config();
        env
    }

    pub fn root(&self) -> &Path {
        &self.root
    }

    pub fn profiles_dir(&self, host: &str) -> PathBuf {
        self.expected.join("hosts").join(host).join("profiles")
    }

    pub fn write_profile(&self, host: &str, name: &str, text: &str) -> &Self {
        let dir = self.profiles_dir(host);
        fs::create_dir_all(&dir).unwrap();
        fs::write(dir.join(format!("{name}.yaml")), text).unwrap();
        self
    }

    pub fn response_dir(&self) -> PathBuf {
        let dir = self.root.join("responses");
        fs::create_dir_all(&dir).unwrap();
        dir
    }

    pub fn write_response(&self, host: &str, key: &str, stdout: &str) -> &Self {
        let dir = self.response_dir();
        fs::write(dir.join(format!("{host}.{key}.stdout")), stdout).unwrap();
        self
    }

    pub fn write_response_file(&self, host: &str, key: &str, path: &Path) -> &Self {
        let dir = self.response_dir();
        fs::write(
            dir.join(format!("{host}.{key}.stdout")),
            fs::read(path).unwrap(),
        )
        .unwrap();
        self
    }

    pub fn write_exit(&self, host: &str, key: &str, code: i32) -> &Self {
        let dir = self.response_dir();
        fs::write(dir.join(format!("{host}.{key}.exit")), code.to_string()).unwrap();
        self
    }

    pub fn write_stderr(&self, host: &str, key: &str, stderr: &str) -> &Self {
        let dir = self.response_dir();
        fs::write(dir.join(format!("{host}.{key}.stderr")), stderr).unwrap();
        self
    }

    pub fn write_timeout_sleep(&self, host: &str, key: &str, seconds: f64) -> &Self {
        let dir = self.response_dir();
        fs::write(
            dir.join(format!("{host}.{key}.timeout")),
            seconds.to_string(),
        )
        .unwrap();
        self
    }

    pub fn write_sleep(&self, host: &str, key: &str, seconds: f64) -> &Self {
        let dir = self.response_dir();
        fs::write(dir.join(format!("{host}.{key}.sleep")), seconds.to_string()).unwrap();
        self
    }

    /// Let the fake ssh exit while a descendant keeps its stdout/stderr pipes
    /// open for the given number of seconds.
    pub fn write_hold_pipe(&self, host: &str, key: &str, seconds: f64) -> &Self {
        let dir = self.response_dir();
        fs::write(
            dir.join(format!("{host}.{key}.hold_pipe")),
            seconds.to_string(),
        )
        .unwrap();
        self
    }

    pub fn actual_stdin(&self, host: &str, key: &str) -> String {
        let path = self
            .response_dir()
            .join("actual")
            .join(format!("{host}.{key}.stdin"));
        fs::read_to_string(path).unwrap_or_default()
    }

    pub fn argv_log(&self, host: &str) -> String {
        fs::read_to_string(self.root.join("responses").join(format!("{host}.argv.log")))
            .unwrap_or_default()
    }

    pub fn stdin_exists(&self, host: &str, key: &str) -> bool {
        self.response_dir()
            .join("actual")
            .join(format!("{host}.{key}.stdin"))
            .exists()
    }

    pub fn run(&self, args: &[&str], extra_env: &[(&str, &str)], stdin: Stdio) -> Output {
        let mut command = Command::new(env!("CARGO_BIN_EXE_gha-fleet"));
        command
            .arg("--dir")
            .arg(&self.expected)
            .args(args)
            .current_dir(&self.root)
            .env(
                "PATH",
                format!("{}:{}", self.fake.display(), env::var("PATH").unwrap()),
            )
            .env("FAKE_SSH_DIR", self.root.join("responses"))
            .stdin(stdin)
            .stdout(Stdio::piped())
            .stderr(Stdio::piped());
        for (key, value) in extra_env {
            command.env(key, value);
        }
        command.output().expect("run gha-fleet")
    }

    pub fn run_yes(&self, args: &[&str], extra_env: &[(&str, &str)]) -> Output {
        self.run(args, extra_env, Stdio::null())
    }

    fn write_config(&self) {
        let mut text = format!("state_dir = {:?}\n", self.state.to_string_lossy());
        for host in &self.hosts {
            text.push_str(&format!("\n[hosts.{}]\n", host.name));
            text.push_str(&format!("ssh = {:?}\n", host.ssh));
            text.push_str("cli = \"/opt/gha-runner-tui/4469c7b/bin/gha-runner-tui\"\n");
            text.push_str("config = \"/etc/gha-runner-tui/config-live-20261117.yaml\"\n");
            text.push_str(&format!("timeout_seconds = {}\n", host.timeout_seconds));
            if let Some(machine_id) = &host.machine_id {
                text.push_str(&format!("instabox_machine_id = {machine_id:?}\n"));
            }
            if let Some(key_env) = &host.api_key_env {
                text.push_str(&format!("instabox_api_key_env = {key_env:?}\n"));
            }
        }
        fs::write(self.expected.join("fleet.toml"), text).unwrap();
    }

    fn write_ssh_shim(&self) {
        let path = self.fake.join("ssh");
        let script = r#"#!/bin/sh
set -u
dir="$FAKE_SSH_DIR"
target=""
seen_dashdash=0
for arg in "$@"; do
  if [ "$seen_dashdash" = "1" ]; then target="$arg"; break; fi
  if [ "$arg" = "--" ]; then seen_dashdash=1; fi
done
cmd=""
dry=0
for arg in "$@"; do
  case "$arg" in
    "'--dry-run'") dry=1 ;;
    "'status'") cmd=status ;;
    "'export'") cmd=export ;;
    "'apply'") cmd=apply ;;
    "'adopt'") cmd=adopt ;;
    "'mode'") cmd=mode ;;
    "'start'") cmd=start ;;
    "'stop'") cmd=stop ;;
    "'restart'") cmd=restart ;;
  esac
done
if [ -z "$target" ] || [ -z "$cmd" ]; then
  echo "fake ssh: cannot identify target or command" >&2
  exit 250
fi
phase=execute
if [ "$cmd" = "apply" ] || [ "$cmd" = "adopt" ]; then
  if [ "$dry" = "1" ]; then phase=preview; fi
  key="$target.$cmd.$phase"
else
  key="$target.$cmd"
fi
mkdir -p "$dir/actual"
{
  echo "CALL $key"
  for arg in "$@"; do printf 'ARG %s\n' "$arg"; done
} >> "$dir/$target.argv.log"
base="$dir/$key"
if [ -f "$base.late_read" ]; then
  exec 3<&0
  (
    sleep "$(cat "$base.late_read")"
    cat > "$dir/actual/$key.stdin" || exit 1
    (printf 'late stdout\n') && : > "$base.stdout_open"
    (printf 'late stderr\n' >&2) && : > "$base.stderr_open"
    : > "$base.late_done"
  ) <&3 3<&- &
  exit 0
fi
cat > "$dir/actual/$key.stdin"
if [ -f "$base.timeout" ]; then
  exec sleep "$(cat "$base.timeout")"
fi
if [ -f "$base.sleep" ]; then sleep "$(cat "$base.sleep")"; fi
if [ -f "$base.hold_pipe" ]; then
  sleep "$(cat "$base.hold_pipe")" &
  exit 0
fi
if [ -f "$base.stderr" ]; then cat "$base.stderr" >&2; fi
if [ -f "$base.stdout" ]; then cat "$base.stdout"; fi
if [ -f "$base.exit" ]; then exit "$(cat "$base.exit")"; fi
exit 0
"#;
        fs::write(&path, script).unwrap();
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            let mut perms = fs::metadata(&path).unwrap().permissions();
            perms.set_mode(0o755);
            fs::set_permissions(&path, perms).unwrap();
        }
    }
}

impl Drop for TestEnv {
    fn drop(&mut self) {
        let _ = fs::remove_dir_all(&self.root);
    }
}

fn temp_root(tag: &str) -> PathBuf {
    let nanos = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap()
        .as_nanos();
    let id = COUNTER.fetch_add(1, Ordering::SeqCst);
    let path = env::temp_dir().join(format!("{tag}-{nanos}-{id}"));
    fs::create_dir_all(&path).unwrap();
    path
}

pub fn fixture(name: &str) -> PathBuf {
    Path::new(env!("CARGO_MANIFEST_DIR"))
        .join("tests")
        .join("fixtures")
        .join(name)
}

pub fn read_fixture(name: &str) -> String {
    fs::read_to_string(fixture(name)).expect("fixture")
}

/// Small local HTTP server for the instabox gate tests.
pub struct MockApi {
    pub base: String,
    requests: Arc<Mutex<Vec<String>>>,
    stop: Arc<AtomicBool>,
    handle: Option<thread::JoinHandle<()>>,
}

impl MockApi {
    pub fn start(body: &str) -> Self {
        Self::start_response("200 OK", "", body)
    }

    pub fn start_status(status_line: &str, body: &str) -> Self {
        Self::start_response(status_line, "", body)
    }

    pub fn start_redirect(location: &str) -> Self {
        Self::start_response("302 Found", &format!("Location: {location}\r\n"), "")
    }

    fn start_response(status_line: &str, extra_headers: &str, body: &str) -> Self {
        let listener = TcpListener::bind("127.0.0.1:0").expect("bind mock api");
        let addr = listener.local_addr().unwrap();
        listener.set_nonblocking(true).unwrap();
        let status_line = status_line.to_string();
        let extra_headers = extra_headers.to_string();
        let body = body.to_string();
        let requests = Arc::new(Mutex::new(Vec::new()));
        let stop = Arc::new(AtomicBool::new(false));
        let requests_for_thread = Arc::clone(&requests);
        let stop_for_thread = Arc::clone(&stop);
        let handle = thread::spawn(move || {
            while !stop_for_thread.load(Ordering::SeqCst) {
                match listener.accept() {
                    Ok((stream, _)) => {
                        handle_request(
                            stream,
                            &status_line,
                            &extra_headers,
                            &body,
                            &requests_for_thread,
                        );
                    }
                    Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => {
                        thread::sleep(Duration::from_millis(5));
                    }
                    Err(_) => break,
                }
            }
        });
        MockApi {
            base: format!("http://{addr}"),
            requests,
            stop,
            handle: Some(handle),
        }
    }

    pub fn requests(&self) -> Vec<String> {
        self.requests.lock().unwrap().clone()
    }
}

impl Drop for MockApi {
    fn drop(&mut self) {
        self.stop.store(true, Ordering::SeqCst);
        let _ = TcpStream::connect(self.base.trim_start_matches("http://"));
        if let Some(handle) = self.handle.take() {
            let _ = handle.join();
        }
    }
}

fn handle_request(
    mut stream: TcpStream,
    status_line: &str,
    extra_headers: &str,
    body: &str,
    requests: &Arc<Mutex<Vec<String>>>,
) {
    let _ = stream.set_read_timeout(Some(Duration::from_millis(250)));
    let mut request = String::new();
    let mut buffer = [0_u8; 1024];
    let deadline = Instant::now() + Duration::from_secs(5);
    while !request.contains("\r\n\r\n") && Instant::now() < deadline {
        match stream.read(&mut buffer) {
            Ok(0) => break,
            Ok(n) => {
                request.push_str(&String::from_utf8_lossy(&buffer[..n]));
                if request.len() > 8192 {
                    break;
                }
            }
            Err(e)
                if matches!(
                    e.kind(),
                    std::io::ErrorKind::WouldBlock
                        | std::io::ErrorKind::TimedOut
                        | std::io::ErrorKind::Interrupted
                ) =>
            {
                thread::sleep(Duration::from_millis(5));
            }
            Err(_) => break,
        }
    }
    requests
        .lock()
        .unwrap()
        .push(request.lines().next().unwrap_or("").to_string());
    let response = format!(
        "HTTP/1.1 {status_line}\r\nContent-Type: application/json\r\n{extra_headers}Content-Length: {}\r\nConnection: close\r\n\r\n{}",
        body.len(),
        body
    );
    let _ = stream.write_all(response.as_bytes());
    let _ = stream.flush();
}

pub fn wait_for<F: Fn() -> bool>(timeout: Duration, f: F) -> bool {
    let started = Instant::now();
    while started.elapsed() < timeout {
        if f() {
            return true;
        }
        thread::sleep(Duration::from_millis(10));
    }
    f()
}

pub fn parse_hosts_json(output: &str) -> serde_json::Value {
    serde_json::from_str(output).expect("valid hosts JSON")
}

pub fn host_reachability<'a>(json: &'a serde_json::Value, host: &str) -> &'a str {
    json["hosts"]
        .as_array()
        .unwrap()
        .iter()
        .find(|entry| entry["name"] == host)
        .and_then(|entry| entry["reachability"].as_str())
        .unwrap_or("missing")
}

pub fn host_error(json: &serde_json::Value, host: &str) -> Option<String> {
    json["hosts"]
        .as_array()
        .unwrap()
        .iter()
        .find(|entry| entry["name"] == host)
        .and_then(|entry| entry["error"].as_str())
        .map(str::to_string)
}

pub fn ordered_pairs(pairs: &[(&str, &str)]) -> BTreeMap<String, String> {
    pairs
        .iter()
        .map(|(key, value)| (key.to_string(), value.to_string()))
        .collect()
}
