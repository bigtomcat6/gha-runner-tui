//! Argument parsing and command dispatch.

use std::collections::BTreeMap;
use std::io::IsTerminal;
use std::path::PathBuf;

use crate::commands::{self, Context, RuntimeOp};
use crate::config::{self, FleetConfig, HostConfig};
use crate::gate::Gate;
use crate::state::ExpectedState;

pub const DEFAULT_DIR: &str = "/etc/gha-fleet";
pub const DIR_ENV: &str = "GHA_FLEET_DIR";

const USAGE: &str = "\
gha-fleet — manage gha-runner-tui profiles across hosts

Usage: gha-fleet <command> [options]

Commands:
  status [--host H]... [--json] [--cached]
  plan [--host H]... [--prune]
  apply [--host H]... [--prune] [--create-only] [--yes]
  adopt --match GLOB [--host H]... [--owner fleet|local] [--yes]
  import [--host H]... [--force]
  mode --match GLOB <0|1|2> [--delay-seconds N] [--host H]...
  start|stop|restart --match GLOB [--host H]...

Global options:
  --dir PATH       expected-state directory (default $GHA_FLEET_DIR or /etc/gha-fleet)
  --host H         restrict to host H; repeatable; 'all' or omitted means every host
";

#[derive(Debug)]
struct Args {
    dir: PathBuf,
    hosts: Vec<String>,
    command: Command,
}

#[derive(Debug)]
enum Command {
    Help,
    Status {
        json: bool,
        cached: bool,
    },
    Plan {
        prune: bool,
    },
    Apply {
        prune: bool,
        create_only: bool,
        yes: bool,
    },
    Adopt {
        glob: String,
        owner: String,
        yes: bool,
    },
    Import {
        force: bool,
    },
    Runtime {
        op: RuntimeOp,
        glob: String,
        mode: Option<u8>,
        delay_seconds: Option<u64>,
    },
}

pub fn run() -> i32 {
    let args: Vec<String> = std::env::args().skip(1).collect();
    let parsed = match parse_args(&args) {
        Ok(args) => args,
        Err(message) => {
            eprintln!("gha-fleet: {message}");
            eprintln!("{USAGE}");
            return 2;
        }
    };
    if matches!(parsed.command, Command::Help) {
        print!("{USAGE}");
        return 0;
    }
    let dir = parsed.dir;
    let config = match config::load(&dir) {
        Ok(config) => config,
        Err(message) => {
            eprintln!("gha-fleet: {message}");
            return 1;
        }
    };
    let command = parsed.command;
    let selected = match select_hosts(&config, &parsed.hosts) {
        Ok(hosts) => hosts,
        Err(message) => {
            eprintln!("gha-fleet: {message}");
            return 2;
        }
    };
    let gate = if selected
        .iter()
        .any(|host| host.instabox_machine_id.is_some())
    {
        match Gate::from_env() {
            Ok(gate) => Some(gate),
            Err(message) => {
                eprintln!("gha-fleet: {message}");
                return 1;
            }
        }
    } else {
        None
    };
    let expected = ExpectedState::new(dir);
    let ctx = Context {
        config,
        expected,
        gate,
        stdin_is_terminal: std::io::stdin().is_terminal(),
    };
    if !matches!(command, Command::Status { cached: true, .. }) {
        let ungated = commands::selected_gate_note(&selected);
        if let Some(ungated) = &ungated {
            commands::print_gate_note(ungated);
        }
    }
    match command {
        Command::Help => 0,
        Command::Status { json, cached } => {
            if cached {
                commands::status_cached(&ctx, &selected, json)
            } else {
                commands::status(&ctx, &selected, json)
            }
        }
        Command::Plan { prune } => commands::plan(&ctx, &selected, prune),
        Command::Apply {
            prune,
            create_only,
            yes,
        } => commands::apply(&ctx, &selected, prune, create_only, yes),
        Command::Adopt { glob, owner, yes } => commands::adopt(&ctx, &selected, &glob, &owner, yes),
        Command::Import { force } => commands::import(&ctx, &selected, force),
        Command::Runtime {
            op,
            glob,
            mode,
            delay_seconds,
        } => commands::runtime(&ctx, &selected, op, &glob, mode, delay_seconds),
    }
}

fn parse_args(args: &[String]) -> Result<Args, String> {
    let mut dir: Option<PathBuf> = None;
    let mut hosts = Vec::new();
    let mut command: Option<String> = None;
    let mut positionals: Vec<String> = Vec::new();
    let mut json = false;
    let mut cached = false;
    let mut prune = false;
    let mut create_only = false;
    let mut yes = false;
    let mut force = false;
    let mut glob: Option<String> = None;
    let mut owner: Option<String> = None;
    let mut delay_seconds: Option<u64> = None;

    let mut index = 0;
    while index < args.len() {
        let arg = &args[index];
        index += 1;
        if arg == "-h" || arg == "--help" || arg == "help" {
            return Ok(Args {
                dir: dir.unwrap_or_else(default_dir),
                hosts,
                command: Command::Help,
            });
        }
        if arg == "--" {
            positionals.extend(args[index..].iter().cloned());
            break;
        }
        if arg == "--dir" {
            let value = next_value(args, &mut index, "--dir")?;
            if value.trim().is_empty() {
                return Err("--dir must not be empty".into());
            }
            if dir.is_some() {
                return Err("--dir was given more than once".into());
            }
            dir = Some(PathBuf::from(value));
            continue;
        }
        if arg == "--host" {
            let value = next_value(args, &mut index, "--host")?;
            hosts.push(value);
            continue;
        }
        if arg == "--json" {
            json = true;
            continue;
        }
        if arg == "--cached" {
            cached = true;
            continue;
        }
        if arg == "--prune" {
            prune = true;
            continue;
        }
        if arg == "--create-only" {
            create_only = true;
            continue;
        }
        if arg == "--yes" {
            yes = true;
            continue;
        }
        if arg == "--force" {
            force = true;
            continue;
        }
        if arg == "--match" {
            let value = next_value(args, &mut index, "--match")?;
            if glob.is_some() {
                return Err("--match was given more than once".into());
            }
            glob = Some(value);
            continue;
        }
        if arg == "--owner" {
            let value = next_value(args, &mut index, "--owner")?;
            if owner.is_some() {
                return Err("--owner was given more than once".into());
            }
            owner = Some(value);
            continue;
        }
        if arg == "--delay-seconds" {
            let value = next_value(args, &mut index, "--delay-seconds")?;
            let parsed = value.parse::<u64>().map_err(|_| {
                format!("--delay-seconds must be a non-negative integer, got {value:?}")
            })?;
            delay_seconds = Some(parsed);
            continue;
        }
        if arg.starts_with('-') && arg != "-" {
            return Err(format!("unknown option {arg:?}"));
        }
        if command.is_none() {
            command = Some(arg.clone());
        } else {
            positionals.push(arg.clone());
        }
    }

    let command_name = command.ok_or_else(|| "command is required".to_string())?;
    let dir = dir.unwrap_or_else(default_dir);
    let command = match command_name.as_str() {
        "status" => {
            if prune
                || create_only
                || yes
                || force
                || glob.is_some()
                || owner.is_some()
                || delay_seconds.is_some()
            {
                return Err("status does not accept plan/apply/adopt options".into());
            }
            if !positionals.is_empty() {
                return Err("status does not accept positional arguments".into());
            }
            Command::Status { json, cached }
        }
        "plan" => {
            if json
                || cached
                || create_only
                || yes
                || force
                || glob.is_some()
                || owner.is_some()
                || delay_seconds.is_some()
            {
                return Err("plan accepts only --host and --prune".into());
            }
            if !positionals.is_empty() {
                return Err("plan does not accept positional arguments".into());
            }
            Command::Plan { prune }
        }
        "apply" => {
            if create_only && prune {
                return Err("apply does not accept --create-only with --prune".into());
            }
            if json
                || cached
                || force
                || glob.is_some()
                || owner.is_some()
                || delay_seconds.is_some()
            {
                return Err("apply accepts only --host, --prune, --create-only and --yes".into());
            }
            if !positionals.is_empty() {
                return Err("apply does not accept positional arguments".into());
            }
            Command::Apply {
                prune,
                create_only,
                yes,
            }
        }
        "adopt" => {
            if json || cached || prune || create_only || force || delay_seconds.is_some() {
                return Err("adopt accepts only --host, --match, --owner and --yes".into());
            }
            if !positionals.is_empty() {
                return Err("adopt does not accept positional profile names".into());
            }
            let glob = glob.ok_or_else(|| "adopt requires --match GLOB".to_string())?;
            commands::validate_glob(&glob)?;
            let owner = match owner.as_deref().unwrap_or("fleet") {
                "fleet" | "local" => owner.unwrap_or_else(|| "fleet".into()),
                other => return Err(format!("--owner must be fleet or local, got {other:?}")),
            };
            Command::Adopt { glob, owner, yes }
        }
        "import" => {
            if json
                || cached
                || prune
                || create_only
                || yes
                || glob.is_some()
                || owner.is_some()
                || delay_seconds.is_some()
            {
                return Err("import accepts only --host and --force".into());
            }
            if !positionals.is_empty() {
                return Err("import does not accept positional arguments".into());
            }
            Command::Import { force }
        }
        "mode" => {
            if json || cached || prune || create_only || yes || force || owner.is_some() {
                return Err("mode accepts only --host, --match and --delay-seconds".into());
            }
            let glob = glob.ok_or_else(|| "mode requires --match GLOB".to_string())?;
            commands::validate_glob(&glob)?;
            if positionals.len() != 1 {
                return Err("mode requires exactly one positional argument: 0, 1 or 2".into());
            }
            let mode = match positionals[0].as_str() {
                "0" => 0,
                "1" => 1,
                "2" => 2,
                other => return Err(format!("mode must be 0, 1 or 2, got {other:?}")),
            };
            if delay_seconds.is_some() && mode != 2 {
                return Err("--delay-seconds requires mode 2".into());
            }
            Command::Runtime {
                op: RuntimeOp::Mode,
                glob,
                mode: Some(mode),
                delay_seconds,
            }
        }
        "start" | "stop" | "restart" => {
            if json
                || cached
                || prune
                || create_only
                || yes
                || force
                || owner.is_some()
                || delay_seconds.is_some()
            {
                return Err(format!("{command_name} accepts only --host and --match"));
            }
            if !positionals.is_empty() {
                return Err(format!(
                    "{command_name} does not accept positional arguments"
                ));
            }
            let glob = glob.ok_or_else(|| format!("{command_name} requires --match GLOB"))?;
            commands::validate_glob(&glob)?;
            let op = match command_name.as_str() {
                "start" => RuntimeOp::Start,
                "stop" => RuntimeOp::Stop,
                _ => RuntimeOp::Restart,
            };
            Command::Runtime {
                op,
                glob,
                mode: None,
                delay_seconds: None,
            }
        }
        other => return Err(format!("unknown command {other:?}")),
    };
    Ok(Args {
        dir,
        hosts,
        command,
    })
}

fn next_value(args: &[String], index: &mut usize, option: &str) -> Result<String, String> {
    let value = args
        .get(*index)
        .ok_or_else(|| format!("{option} needs a value"))?;
    *index += 1;
    Ok(value.clone())
}

fn default_dir() -> PathBuf {
    match std::env::var(DIR_ENV) {
        Ok(value) if !value.trim().is_empty() => PathBuf::from(value),
        _ => PathBuf::from(DEFAULT_DIR),
    }
}

/// Resolve --host (repeatable, "all" = every configured host).
fn select_hosts(config: &FleetConfig, names: &[String]) -> Result<Vec<HostConfig>, String> {
    if names.is_empty() || names.iter().any(|name| name == "all") {
        return Ok(config.hosts.values().cloned().collect());
    }
    let mut selected = BTreeMap::new();
    for name in names {
        let host = config
            .hosts
            .get(name)
            .ok_or_else(|| format!("unknown host {name:?} (not in fleet.toml)"))?;
        selected.insert(name.clone(), host.clone());
    }
    Ok(selected.into_values().collect())
}
