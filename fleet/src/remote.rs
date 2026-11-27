//! Running the node CLI over SSH.

use std::io::{Read, Write};
use std::process::{Command, ExitStatus, Stdio};
use std::time::{Duration, Instant};

use crate::config::HostConfig;

pub const CONNECT_TIMEOUT_SECONDS: u64 = 10;

#[derive(Debug, Clone)]
pub struct RawReply {
    pub exit_code: i32,
    pub stdout: String,
    pub stderr: String,
}

#[derive(Debug, Clone)]
pub enum RemoteError {
    Spawn(String),
    Timeout {
        seconds: u64,
        stderr: String,
    },
    Signal(Option<i32>),
    /// The node's stdout could not be decoded or was not valid UTF-8.
    InvalidOutput(String),
    /// Provably failed before the remote command could run.
    PreDispatch(String),
}

/// Single-quote an argv element for the remote POSIX shell.
pub fn shell_quote(arg: &str) -> String {
    let mut out = String::with_capacity(arg.len() + 2);
    out.push('\'');
    for c in arg.chars() {
        if c == '\'' {
            out.push_str("'\\''");
        } else {
            out.push(c);
        }
    }
    out.push('\'');
    out
}

pub fn execute(
    host: &HostConfig,
    remote_args: &[String],
    stdin: Option<&str>,
) -> Result<RawReply, RemoteError> {
    let mut command = Command::new("ssh");
    command
        .arg("-o")
        .arg("BatchMode=yes")
        .arg("-o")
        .arg(format!("ConnectTimeout={CONNECT_TIMEOUT_SECONDS}"))
        .arg("--")
        .arg(&host.ssh)
        .arg("sudo")
        .arg("-n")
        .arg(shell_quote(&host.cli));
    for arg in remote_args {
        command.arg(shell_quote(arg));
    }
    command
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped());

    let mut child = command
        .spawn()
        .map_err(|e| RemoteError::Spawn(format!("spawn ssh: {e}")))?;
    let mut child_stdin = child.stdin.take();
    let stdout = child.stdout.take();
    let stderr = child.stderr.take();

    let input = stdin.map(|s| s.as_bytes().to_vec());
    let writer = std::thread::spawn(move || {
        if let Some(input) = input {
            if let Some(stdin) = child_stdin.as_mut() {
                let _ = stdin.write_all(&input);
            }
        }
        drop(child_stdin);
    });
    let stdout_reader = stdout.map(|mut pipe| {
        std::thread::spawn(move || {
            let mut buf = Vec::new();
            let _ = pipe.read_to_end(&mut buf);
            buf
        })
    });
    let stderr_reader = stderr.map(|mut pipe| {
        std::thread::spawn(move || {
            let mut buf = Vec::new();
            let _ = pipe.read_to_end(&mut buf);
            buf
        })
    });

    let timeout = Duration::from_secs(host.timeout_seconds);
    let started = Instant::now();
    let status = loop {
        match child.try_wait() {
            Ok(Some(status)) => break Some(status),
            Ok(None) => {
                if started.elapsed() >= timeout {
                    break None;
                }
                std::thread::sleep(Duration::from_millis(10));
            }
            Err(e) => {
                let _ = child.kill();
                let _ = child.wait();
                let _ = writer.join();
                if let Some(reader) = stdout_reader {
                    let _ = reader.join();
                }
                if let Some(reader) = stderr_reader {
                    let _ = reader.join();
                }
                return Err(RemoteError::Spawn(format!("wait for ssh: {e}")));
            }
        }
    };

    let timed_out = status.is_none();
    if timed_out {
        let _ = child.kill();
    }
    let _ = child.wait();
    let _ = writer.join();
    let stdout_bytes = stdout_reader
        .map(|reader| reader.join().unwrap_or_default())
        .unwrap_or_default();
    let stderr_bytes = stderr_reader
        .map(|reader| reader.join().unwrap_or_default())
        .unwrap_or_default();
    let stderr_text = String::from_utf8_lossy(&stderr_bytes).into_owned();

    if timed_out {
        return Err(RemoteError::Timeout {
            seconds: host.timeout_seconds,
            stderr: stderr_text,
        });
    }
    let status: ExitStatus = status.expect("checked above");
    let Some(exit_code) = status.code() else {
        return Err(RemoteError::Signal(signal_of(&status)));
    };
    let stdout_text = match String::from_utf8(stdout_bytes) {
        Ok(text) => text,
        Err(_) => {
            return Err(RemoteError::InvalidOutput(
                "node stdout is not valid UTF-8".to_string(),
            ))
        }
    };
    if exit_code == 255 && stdout_text.is_empty() && proves_pre_dispatch(&stderr_text) {
        return Err(RemoteError::PreDispatch(stderr_text));
    }

    Ok(RawReply {
        exit_code,
        stdout: stdout_text,
        stderr: stderr_text,
    })
}

/// Positive evidence that ssh never reached the remote command. Exit 255 with
/// no output alone does not prove it: only a recognized client-side
/// connect/authentication failure is treated as pre-dispatch. An ambiguous
/// 255 (for example, a session that authenticated and then dropped) stays
/// `unknown`.
fn proves_pre_dispatch(stderr: &str) -> bool {
    const NEEDLES: &[&str] = &[
        "ssh: Could not resolve hostname",
        "ssh: connect to host",
        "No route to host",
        "Network is unreachable",
        "Permission denied (",
        "Host key verification failed",
        "kex_exchange_identification:",
        "ssh_exchange_identification:",
    ];
    NEEDLES.iter().any(|needle| stderr.contains(needle))
}

#[cfg(unix)]
fn signal_of(status: &ExitStatus) -> Option<i32> {
    use std::os::unix::process::ExitStatusExt;
    status.signal()
}

#[cfg(not(unix))]
fn signal_of(_status: &ExitStatus) -> Option<i32> {
    None
}

pub fn stderr_tail(stderr: &str) -> Option<String> {
    stderr
        .lines()
        .map(str::trim)
        .filter(|line| !line.is_empty())
        .filter(|line| !line.starts_with("debug1:"))
        .filter(|line| !line.starts_with("OpenSSH_"))
        .next_back()
        .map(|line| line.chars().take(300).collect())
}

#[cfg(test)]
mod tests {
    use super::{proves_pre_dispatch, shell_quote};

    #[test]
    fn quotes_plain_and_embedded_single_quotes() {
        assert_eq!(shell_quote("abc"), "'abc'");
        assert_eq!(shell_quote("a b"), "'a b'");
        assert_eq!(shell_quote("a'b"), "'a'\\''b'");
        assert_eq!(shell_quote("$(x)"), "'$(x)'");
    }

    #[test]
    fn only_named_client_failures_prove_pre_dispatch() {
        for line in [
            "ssh: Could not resolve hostname s1: nodename nor servname provided",
            "ssh: connect to host s1 port 22: Connection refused",
            "Permission denied (publickey).",
            "Host key verification failed.",
            "kex_exchange_identification: Connection closed by remote host",
        ] {
            assert!(proves_pre_dispatch(line), "{line}");
        }
        assert!(!proves_pre_dispatch(""));
        assert!(!proves_pre_dispatch(
            "debug1: Authenticated to s1 ([10.0.0.1]:22)"
        ));
    }
}
