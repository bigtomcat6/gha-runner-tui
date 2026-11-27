//! Running the node CLI over SSH.

use std::io::{Read, Write};
use std::process::{Command, ExitStatus, Stdio};
use std::sync::mpsc::{self, Receiver};
use std::time::{Duration, Instant};

use crate::config::HostConfig;

pub const CONNECT_TIMEOUT_SECONDS: u64 = 10;

/// OpenSSH uses 255 for its own failures, and also forwards a remote command
/// that itself exited 255. The status alone therefore cannot prove that the
/// remote command did not run.
pub const SSH_CLIENT_FAILURE_EXIT_CODE: i32 = 255;

#[derive(Debug, Clone)]
pub struct RawReply {
    pub exit_code: i32,
    pub stdout: String,
    pub stderr: String,
}

#[derive(Debug, Clone)]
pub enum RemoteError {
    /// `ssh` could not be started locally; no remote command was dispatched.
    Spawn(String),
    Timeout {
        seconds: u64,
        stderr: String,
    },
    Signal(Option<i32>),
    /// The node's stdout could not be decoded or was not valid UTF-8.
    InvalidOutput(String),
    /// ssh itself exited 255. ssh shares its stderr with the remote command and
    /// reports remote exit 255 the same way, so no output can prove that the
    /// remote command never started. Every 255 stays unknown.
    SshClientFailure {
        stderr: String,
    },
    /// Waiting for the local ssh process failed after it was spawned.
    Wait(String),
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
    let (writer_done_tx, writer_done_rx) = mpsc::channel();
    let _writer = std::thread::spawn(move || {
        if let Some(input) = input {
            if let Some(stdin) = child_stdin.as_mut() {
                let _ = stdin.write_all(&input);
            }
        }
        drop(child_stdin);
        let _ = writer_done_tx.send(());
    });
    let stdout_rx = stdout.map(|mut pipe| {
        let (tx, rx) = mpsc::channel();
        let _ = std::thread::spawn(move || {
            let mut buf = Vec::new();
            let _ = pipe.read_to_end(&mut buf);
            let _ = tx.send(buf);
        });
        rx
    });
    let stderr_rx = stderr.map(|mut pipe| {
        let (tx, rx) = mpsc::channel();
        let _ = std::thread::spawn(move || {
            let mut buf = Vec::new();
            let _ = pipe.read_to_end(&mut buf);
            let _ = tx.send(buf);
        });
        rx
    });

    let timeout = Duration::from_secs(host.timeout_seconds);
    let deadline = Instant::now() + timeout;
    let status = loop {
        match child.try_wait() {
            Ok(Some(status)) => break Some(status),
            Ok(None) => {
                let now = Instant::now();
                if now >= deadline {
                    break None;
                }
                std::thread::sleep((deadline - now).min(Duration::from_millis(10)));
            }
            Err(e) => {
                let _ = child.kill();
                let _ = child.wait();
                return Err(RemoteError::Wait(format!("wait for ssh: {e}")));
            }
        }
    };

    let timed_out = status.is_none();
    if timed_out {
        let _ = child.kill();
    }
    let _ = child.wait();

    // The ssh child can exit while a descendant still holds its stdout/stderr
    // pipes open. Waiting for those readers must share the same deadline as
    // the child; a timed-out reader thread is detached, never joined forever.
    let mut pipes_timed_out = false;
    let stdout_bytes = match stdout_rx {
        Some(rx) => match recv_until(&rx, deadline) {
            Some(bytes) => bytes,
            None => {
                pipes_timed_out = true;
                Vec::new()
            }
        },
        None => Vec::new(),
    };
    let stderr_bytes = match stderr_rx {
        Some(rx) => match recv_until(&rx, deadline) {
            Some(bytes) => bytes,
            None => {
                pipes_timed_out = true;
                Vec::new()
            }
        },
        None => Vec::new(),
    };
    if recv_until(&writer_done_rx, deadline).is_none() {
        pipes_timed_out = true;
    }
    let stderr_text = String::from_utf8_lossy(&stderr_bytes).into_owned();

    if timed_out || pipes_timed_out {
        return Err(RemoteError::Timeout {
            seconds: host.timeout_seconds,
            stderr: stderr_text,
        });
    }
    let status: ExitStatus = status.expect("checked above");
    let Some(exit_code) = status.code() else {
        return Err(RemoteError::Signal(signal_of(&status)));
    };
    if exit_code == SSH_CLIENT_FAILURE_EXIT_CODE {
        return Err(RemoteError::SshClientFailure {
            stderr: stderr_text,
        });
    }
    let stdout_text = match String::from_utf8(stdout_bytes) {
        Ok(text) => text,
        Err(_) => {
            return Err(RemoteError::InvalidOutput(
                "node stdout is not valid UTF-8".to_string(),
            ))
        }
    };

    Ok(RawReply {
        exit_code,
        stdout: stdout_text,
        stderr: stderr_text,
    })
}

/// Wait for a worker result only until the shared deadline. On expiry the
/// caller treats the pipes as incomplete; the reader thread stays detached.
fn recv_until<T>(rx: &Receiver<T>, deadline: Instant) -> Option<T> {
    let remaining = deadline.saturating_duration_since(Instant::now());
    if remaining.is_zero() {
        return rx.try_recv().ok();
    }
    match rx.recv_timeout(remaining) {
        Ok(value) => Some(value),
        Err(_) => rx.try_recv().ok(),
    }
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
    use super::shell_quote;

    #[test]
    fn quotes_plain_and_embedded_single_quotes() {
        assert_eq!(shell_quote("abc"), "'abc'");
        assert_eq!(shell_quote("a b"), "'a b'");
        assert_eq!(shell_quote("a'b"), "'a'\\''b'");
        assert_eq!(shell_quote("$(x)"), "'$(x)'");
    }
}
