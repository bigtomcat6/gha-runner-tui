//! Local expected-state files and T4 snapshots.

use std::fs;
use std::io::Write;
use std::path::{Path, PathBuf};

use serde::Deserialize;
use serde_json::Value;

use crate::bundle::{read_host_dir, Bundle, BundleState};
use crate::contract::ExportProfile;

pub const STALE_AFTER_SECONDS: i64 = 3 * 300;

#[derive(Debug, Clone)]
pub struct ExpectedState {
    root: PathBuf,
}

impl ExpectedState {
    pub fn new(root: PathBuf) -> Self {
        ExpectedState { root }
    }

    pub fn root_path(&self) -> &Path {
        &self.root
    }

    pub fn profiles_dir(&self, host: &str) -> PathBuf {
        self.root.join("hosts").join(host).join("profiles")
    }

    pub fn read_bundle(&self, host: &str) -> Result<(BundleState, Bundle), String> {
        read_host_dir(&self.profiles_dir(host))
    }
}

/// Validate a profile name received from the node before using it as a local
/// file name. The node's contract only produces plain names; anything that can
/// escape the profiles directory is rejected.
pub fn validate_profile_name(name: &str) -> Result<(), String> {
    if name.is_empty() {
        return Err("profile name is empty".into());
    }
    if name == "." || name == ".." {
        return Err(format!("profile name {name:?} is not a valid file name"));
    }
    if name.starts_with('-') {
        return Err(format!("profile name {name:?} must not start with '-'"));
    }
    if !name
        .chars()
        .all(|c| c.is_ascii_alphanumeric() || matches!(c, '.' | '_' | '-'))
    {
        return Err(format!(
            "profile name {name:?} may contain only [A-Za-z0-9._-]"
        ));
    }
    Ok(())
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ImportAction {
    Create,
    Overwrite,
    Unchanged,
}

#[derive(Debug, Clone)]
pub struct ImportWrite {
    pub name: String,
    pub yaml: String,
    pub action: ImportAction,
}

#[derive(Debug, Clone)]
pub struct ImportPlan {
    pub writes: Vec<ImportWrite>,
    /// Files already in the local profiles directory that the export did not
    /// contain. They are reported, never deleted.
    pub local_only: Vec<String>,
}

/// Validate a whole export and work out exactly what would be written.
pub fn plan_import(
    root: &Path,
    host: &str,
    profiles: &[ExportProfile],
    force: bool,
) -> Result<ImportPlan, String> {
    let dir = root.join("hosts").join(host).join("profiles");
    let mut names = std::collections::BTreeSet::new();
    let mut writes = Vec::new();
    for profile in profiles {
        validate_profile_name(&profile.name)?;
        if !names.insert(profile.name.clone()) {
            return Err(format!(
                "export for host {host} contains duplicate profile name {:?}",
                profile.name
            ));
        }
        if profile.owner != "fleet" {
            return Err(format!(
                "export for host {host} returned profile {:?} with owner {:?}; \
                 a fleet import must contain only fleet-owned profiles",
                profile.name, profile.owner
            ));
        }
        let path = dir.join(format!("{}.yaml", profile.name));
        let action = match fs::read_to_string(&path) {
            Ok(existing) if existing == profile.yaml => ImportAction::Unchanged,
            Ok(_) => {
                if !force {
                    return Err(format!(
                        "local file {} differs from the node export; rerun with --force to overwrite",
                        path.display()
                    ));
                }
                ImportAction::Overwrite
            }
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => ImportAction::Create,
            Err(e) => return Err(format!("read {}: {e}", path.display())),
        };
        writes.push(ImportWrite {
            name: profile.name.clone(),
            yaml: profile.yaml.clone(),
            action,
        });
    }

    let mut local_only = Vec::new();
    match fs::read_dir(&dir) {
        Ok(entries) => {
            for entry in entries {
                let entry = entry.map_err(|e| format!("read {}: {e}", dir.display()))?;
                if !entry
                    .file_type()
                    .map_err(|e| format!("stat {}: {e}", entry.path().display()))?
                    .is_file()
                {
                    continue;
                }
                let name = entry.file_name().to_string_lossy().into_owned();
                let Some(stem) = name.strip_suffix(".yaml") else {
                    continue;
                };
                if !names.contains(stem) {
                    local_only.push(stem.to_string());
                }
            }
        }
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => {}
        Err(e) => return Err(format!("read {}: {e}", dir.display())),
    }
    local_only.sort();
    Ok(ImportPlan { writes, local_only })
}

/// Stage all writes as temporary files, then rename them into place. This
/// keeps a failed preflight from touching any existing expected-state file.
pub fn commit_import(root: &Path, host: &str, plan: &ImportPlan) -> Result<(), String> {
    if plan.writes.is_empty() {
        fs::create_dir_all(root.join("hosts").join(host).join("profiles"))
            .map_err(|e| format!("create expected-state directory for {host}: {e}"))?;
        return Ok(());
    }
    let dir = root.join("hosts").join(host).join("profiles");
    fs::create_dir_all(&dir).map_err(|e| format!("create {}: {e}", dir.display()))?;
    let mut staged = Vec::new();
    for write in &plan.writes {
        if write.action == ImportAction::Unchanged {
            continue;
        }
        let final_path = dir.join(format!("{}.yaml", write.name));
        let temp_path = dir.join(format!(".{}.yaml.gha-fleet-tmp", write.name));
        let write_result = (|| -> std::io::Result<()> {
            let mut file = fs::File::create(&temp_path)?;
            file.write_all(write.yaml.as_bytes())?;
            file.sync_all()?;
            Ok(())
        })();
        if let Err(e) = write_result {
            for (path, _) in &staged {
                let _ = fs::remove_file(path);
            }
            let _ = fs::remove_file(&temp_path);
            return Err(format!("write {}: {e}", temp_path.display()));
        }
        staged.push((temp_path, final_path));
    }
    for (temp, final_path) in staged {
        if let Err(e) = fs::rename(&temp, &final_path) {
            return Err(format!(
                "rename {} to {}: {e}; some staged files may remain",
                temp.display(),
                final_path.display()
            ));
        }
    }
    Ok(())
}

pub fn snapshot_path(state_dir: &Path, host: &str) -> PathBuf {
    state_dir.join("snapshots").join(format!("{host}.json"))
}

#[derive(Debug, Clone, Deserialize)]
pub struct Snapshot {
    #[serde(default)]
    pub last_ok: Option<SnapshotOk>,
    #[serde(default)]
    pub last_attempt: Option<SnapshotAttempt>,
}

#[derive(Debug, Clone, Deserialize)]
pub struct SnapshotOk {
    #[serde(default)]
    pub observed_at: String,
    #[serde(default)]
    pub status: Option<Value>,
}

#[derive(Debug, Clone, Deserialize)]
pub struct SnapshotAttempt {
    #[serde(default)]
    pub at: String,
    #[serde(default)]
    pub reachability: String,
    #[serde(default)]
    pub error: Option<String>,
}

pub fn load_snapshot(state_dir: &Path, host: &str) -> Result<Option<Snapshot>, String> {
    let path = snapshot_path(state_dir, host);
    let text = match fs::read_to_string(&path) {
        Ok(text) => text,
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(None),
        Err(e) => return Err(format!("read {}: {e}", path.display())),
    };
    let snapshot: Snapshot =
        serde_json::from_str(&text).map_err(|e| format!("parse {}: {e}", path.display()))?;
    Ok(Some(snapshot))
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::contract::ExportProfile;
    use std::time::{SystemTime, UNIX_EPOCH};

    fn temp_dir(tag: &str) -> PathBuf {
        let nanos = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .unwrap()
            .as_nanos();
        let dir = std::env::temp_dir().join(format!("gha-fleet-state-{tag}-{nanos}"));
        fs::create_dir_all(&dir).unwrap();
        dir
    }

    fn profile(name: &str, owner: &str, yaml: &str) -> ExportProfile {
        ExportProfile {
            name: name.into(),
            path: format!("/node/{name}.yaml"),
            owner: owner.into(),
            yaml: yaml.into(),
        }
    }

    #[test]
    fn rejects_unsafe_profile_names() {
        for name in ["", ".", "..", "a/b", "../x", "a b", "-x", "a$b"] {
            assert!(validate_profile_name(name).is_err(), "{name:?}");
        }
        assert!(validate_profile_name("a-b_c.1").is_ok());
    }

    #[test]
    fn plans_and_commits_import_atomically() {
        let root = temp_dir("import");
        let profiles = vec![
            profile("a", "fleet", "name: a\nmanaged_by: fleet\n"),
            profile("b", "fleet", "name: b\nmanaged_by: fleet\n"),
        ];
        let plan = plan_import(&root, "s1", &profiles, false).unwrap();
        assert_eq!(plan.writes.len(), 2);
        assert_eq!(plan.writes[0].action, ImportAction::Create);
        commit_import(&root, "s1", &plan).unwrap();
        assert_eq!(
            fs::read_to_string(root.join("hosts/s1/profiles/a.yaml")).unwrap(),
            "name: a\nmanaged_by: fleet\n"
        );
        let same = plan_import(&root, "s1", &profiles, false).unwrap();
        assert_eq!(same.writes[0].action, ImportAction::Unchanged);
        let changed = vec![profile(
            "a",
            "fleet",
            "name: a\nmanaged_by: fleet\n# changed\n",
        )];
        assert!(plan_import(&root, "s1", &changed, false).is_err());
        assert!(plan_import(&root, "s1", &changed, true).is_ok());
        fs::write(root.join("hosts/s1/profiles/local-only.yaml"), "x").unwrap();
        let plan = plan_import(&root, "s1", &profiles, false).unwrap();
        assert_eq!(plan.local_only, vec!["local-only"]);
        fs::remove_dir_all(root).unwrap();
    }

    #[test]
    fn duplicate_and_wrong_owner_are_rejected() {
        let root = temp_dir("dupe");
        let profiles = vec![profile("a", "fleet", "x"), profile("a", "fleet", "y")];
        assert!(plan_import(&root, "s1", &profiles, false).is_err());
        let profiles = vec![profile("a", "local", "x")];
        assert!(plan_import(&root, "s1", &profiles, false).is_err());
        fs::remove_dir_all(root).unwrap();
    }
}
