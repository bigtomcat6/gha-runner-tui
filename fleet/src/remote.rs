//! Running the node CLI over SSH.

use std::io::{self, ErrorKind, Read, Write};
use std::os::fd::AsRawFd;
use std::os::unix::process::ExitStatusExt;
use std::process::{Child, Command, ExitStatus, Stdio};
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
    /// Waiting for or communicating with the local ssh process failed.
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

    let deadline = Instant::now() + Duration::from_secs(host.timeout_seconds);
    let mut child = command
        .spawn()
        .map_err(|e| RemoteError::Spawn(format!("spawn ssh: {e}")))?;
    let mut stdout_bytes = Vec::new();
    let mut stderr_bytes = Vec::new();
    let status = exchange(
        &mut child,
        stdin.unwrap_or_default().as_bytes(),
        deadline,
        &mut stdout_bytes,
        &mut stderr_bytes,
    );
    if !matches!(status, Ok(Some(_))) {
        // The pipes are already closed. Only signal our direct child, and
        // don't extend the deadline waiting for the OS to reap it.
        let _ = child.kill();
        if matches!(child.try_wait(), Ok(None)) {
            std::thread::spawn(move || {
                let _ = child.wait();
            });
        }
    }
    let stderr_text = String::from_utf8_lossy(&stderr_bytes).into_owned();
    let Some(status) =
        status.map_err(|e| RemoteError::Wait(format!("communicate with ssh: {e}")))?
    else {
        return Err(RemoteError::Timeout {
            seconds: host.timeout_seconds,
            stderr: stderr_text,
        });
    };
    let Some(exit_code) = status.code() else {
        return Err(RemoteError::Signal(status.signal()));
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

/// Own all pipe ends here so they close on timeout, even if an SSH descendant
/// keeps the other ends open after the direct child exits.
fn exchange(
    child: &mut Child,
    mut input: &[u8],
    deadline: Instant,
    stdout: &mut Vec<u8>,
    stderr: &mut Vec<u8>,
) -> io::Result<Option<ExitStatus>> {
    let mut stdin_pipe = child.stdin.take().filter(|_| !input.is_empty());
    let mut stdout_pipe = child.stdout.take();
    let mut stderr_pipe = child.stderr.take();
    for fd in [
        pipe_fd(&stdin_pipe),
        pipe_fd(&stdout_pipe),
        pipe_fd(&stderr_pipe),
    ] {
        if fd >= 0 {
            set_nonblocking(fd)?;
        }
    }

    let mut status = None;
    let mut buffer = [0; 16 * 1024];
    while Instant::now() < deadline {
        if status.is_none() {
            status = child.try_wait()?;
        }
        if stdin_pipe.is_none()
            && stdout_pipe.is_none()
            && stderr_pipe.is_none()
            && status.is_some()
        {
            return Ok(status);
        }

        let mut fds = [
            libc::pollfd {
                fd: pipe_fd(&stdin_pipe),
                events: libc::POLLOUT,
                revents: 0,
            },
            libc::pollfd {
                fd: pipe_fd(&stdout_pipe),
                events: libc::POLLIN,
                revents: 0,
            },
            libc::pollfd {
                fd: pipe_fd(&stderr_pipe),
                events: libc::POLLIN,
                revents: 0,
            },
        ];
        // Wake periodically to check the child even if no fd becomes ready.
        let remaining = deadline.saturating_duration_since(Instant::now());
        let wait_ms = remaining.as_millis().clamp(1, 10) as libc::c_int;
        // SAFETY: poll receives a live array of exactly fds.len() entries.
        if unsafe { libc::poll(fds.as_mut_ptr(), fds.len() as libc::nfds_t, wait_ms) } < 0 {
            let error = io::Error::last_os_error();
            if error.kind() == ErrorKind::Interrupted {
                continue;
            }
            return Err(error);
        }
        if Instant::now() >= deadline {
            break;
        }
        if let Some(pipe) = stdin_pipe.as_mut().filter(|_| fds[0].revents != 0) {
            match pipe.write(&input[..input.len().min(buffer.len())]) {
                Ok(0) => stdin_pipe = None,
                Ok(n) => {
                    input = &input[n..];
                    if input.is_empty() {
                        stdin_pipe = None;
                    }
                }
                // The node can reject before reading all input. Keep its reply.
                Err(e) if e.kind() == ErrorKind::BrokenPipe => stdin_pipe = None,
                Err(e) if retry_io(&e) => (),
                Err(e) => return Err(e),
            }
        }
        if fds[1].revents != 0 {
            read_pipe(&mut stdout_pipe, stdout, &mut buffer)?;
        }
        if fds[2].revents != 0 {
            read_pipe(&mut stderr_pipe, stderr, &mut buffer)?;
        }
    }
    Ok(None)
}

fn pipe_fd(pipe: &Option<impl AsRawFd>) -> libc::c_int {
    pipe.as_ref().map_or(-1, AsRawFd::as_raw_fd)
}

fn set_nonblocking(fd: libc::c_int) -> io::Result<()> {
    // SAFETY: fd is a live parent-owned pipe end; these commands take no
    // pointer arguments and only change the flags on that end.
    unsafe {
        let flags = libc::fcntl(fd, libc::F_GETFL);
        if flags < 0 || libc::fcntl(fd, libc::F_SETFL, flags | libc::O_NONBLOCK) < 0 {
            return Err(io::Error::last_os_error());
        }
    }
    Ok(())
}

fn retry_io(error: &io::Error) -> bool {
    matches!(error.kind(), ErrorKind::WouldBlock | ErrorKind::Interrupted)
}

fn read_pipe(
    pipe: &mut Option<impl Read>,
    output: &mut Vec<u8>,
    buffer: &mut [u8],
) -> io::Result<()> {
    if let Some(reader) = pipe {
        match reader.read(buffer) {
            Ok(0) => *pipe = None,
            Ok(n) => output.extend_from_slice(&buffer[..n]),
            Err(e) if retry_io(&e) => (),
            Err(e) => return Err(e),
        }
    }
    Ok(())
}

pub fn stderr_tail(stderr: &str) -> Option<String> {
    stderr
        .lines()
        .map(str::trim)
        .rfind(|line| {
            !line.is_empty() && !line.starts_with("debug1:") && !line.starts_with("OpenSSH_")
        })
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
