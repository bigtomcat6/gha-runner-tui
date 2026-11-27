//! Building the `apply -f -` bundle from the expected-state directory.

use std::fs;
use std::path::Path;

pub const MAX_BUNDLE_BYTES: usize = 1024 * 1024;

#[derive(Debug, Clone, Default)]
pub struct Bundle {
    pub text: String,
    pub profiles: Vec<String>,
}

/// One local expected-state directory for a host.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum BundleState {
    Missing,
    Empty,
    Ready,
}

pub fn read_host_dir(dir: &Path) -> Result<(BundleState, Bundle), String> {
    if !dir.exists() {
        return Ok((BundleState::Missing, Bundle::default()));
    }
    if !dir.is_dir() {
        return Err(format!("{} is not a directory", dir.display()));
    }
    let mut files = Vec::new();
    let entries =
        fs::read_dir(dir).map_err(|e| format!("read directory {}: {e}", dir.display()))?;
    for entry in entries {
        let entry = entry.map_err(|e| format!("read directory {}: {e}", dir.display()))?;
        let name = entry.file_name().to_string_lossy().into_owned();
        let file_type = entry
            .file_type()
            .map_err(|e| format!("stat {}: {e}", entry.path().display()))?;
        if file_type.is_file() {
            if name.ends_with(".yaml") {
                files.push((name, entry.path()));
            }
            continue;
        }
        if name.ends_with(".yaml") {
            return Err(format!(
                "profile {} is not a regular file; symlinks and other special files \
                 are refused in the expected-state directory",
                entry.path().display()
            ));
        }
    }
    files.sort_by(|a, b| a.0.cmp(&b.0));

    let mut docs = Vec::new();
    let mut names = Vec::new();
    for (file_name, path) in files {
        let bytes = fs::read(&path).map_err(|e| format!("read profile {}: {e}", path.display()))?;
        let text = String::from_utf8(bytes)
            .map_err(|_| format!("profile {} is not valid UTF-8", path.display()))?;
        if !declares_fleet_owner(&text) {
            return Err(format!(
                "profile {} does not declare a top-level 'managed_by: fleet' line; \
                 fleet-owned expected state must carry that ownership",
                path.display()
            ));
        }
        let mut doc = text;
        if !doc.ends_with('\n') {
            doc.push('\n');
        }
        docs.push(doc);
        names.push(file_name);
    }
    if docs.is_empty() {
        return Ok((BundleState::Empty, Bundle::default()));
    }
    let text = docs.join("---\n");
    if text.len() > MAX_BUNDLE_BYTES {
        return Err(format!(
            "bundle for {} is {} bytes, exceeding the 1 MiB limit",
            dir.display(),
            text.len()
        ));
    }
    Ok((
        BundleState::Ready,
        Bundle {
            text,
            profiles: names,
        },
    ))
}

/// The center deliberately does not parse YAML. Ownership is a single
/// top-level scalar, so a conservative line scan is enough to catch a wrong
/// expected-state file before the node sees it.
pub fn declares_fleet_owner(text: &str) -> bool {
    let mut saw_any = false;
    for line in text.lines() {
        if line.starts_with(char::is_whitespace) {
            continue;
        }
        let Some(value) = line.strip_prefix("managed_by:") else {
            continue;
        };
        saw_any = true;
        let value = value.trim_start();
        let value = value.split('#').next().unwrap_or("").trim_end();
        let value = value.trim_matches('\'').trim_matches('"');
        if value != "fleet" {
            return false;
        }
    }
    saw_any
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::time::{SystemTime, UNIX_EPOCH};

    fn temp_dir(tag: &str) -> std::path::PathBuf {
        let nanos = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .unwrap()
            .as_nanos();
        let dir = std::env::temp_dir().join(format!("gha-fleet-bundle-{tag}-{nanos}"));
        fs::create_dir_all(&dir).unwrap();
        dir
    }

    #[test]
    fn joins_sorted_documents_and_adds_final_newline() {
        let dir = temp_dir("join");
        fs::write(dir.join("b.yaml"), "name: b\nmanaged_by: fleet").unwrap();
        fs::write(dir.join("a.yaml"), "name: a\nmanaged_by: fleet\n").unwrap();
        fs::write(dir.join("ignored.txt"), "x").unwrap();
        let (state, bundle) = read_host_dir(&dir).unwrap();
        assert_eq!(state, BundleState::Ready);
        assert_eq!(
            bundle.text,
            "name: a\nmanaged_by: fleet\n---\nname: b\nmanaged_by: fleet\n"
        );
        assert_eq!(bundle.profiles, vec!["a.yaml", "b.yaml"]);
        fs::remove_dir_all(dir).unwrap();
    }

    #[test]
    fn missing_and_empty_directories() {
        let missing = temp_dir("missing").join("profiles");
        assert_eq!(read_host_dir(&missing).unwrap().0, BundleState::Missing);
        let empty = temp_dir("empty");
        assert_eq!(read_host_dir(&empty).unwrap().0, BundleState::Empty);
        fs::remove_dir_all(empty).unwrap();
    }

    #[test]
    fn requires_fleet_ownership() {
        assert!(declares_fleet_owner("name: a\nmanaged_by: fleet\n"));
        assert!(declares_fleet_owner("managed_by: fleet # ok\n"));
        assert!(declares_fleet_owner("managed_by: \"fleet\"\n"));
        assert!(!declares_fleet_owner("name: a\n"));
        assert!(!declares_fleet_owner("managed_by: local\n"));
        assert!(!declares_fleet_owner("  managed_by: fleet\n"));
        assert!(!declares_fleet_owner(
            "managed_by: fleet\nmanaged_by: local\n"
        ));
    }

    #[test]
    fn rejects_non_utf8_and_oversized_bundles() {
        let dir = temp_dir("utf8");
        fs::write(dir.join("bad.yaml"), [0xff, 0xfe]).unwrap();
        assert!(read_host_dir(&dir).is_err());
        fs::remove_dir_all(dir).unwrap();
    }

    #[test]
    fn rejects_oversized_bundle() {
        let dir = temp_dir("big");
        let mut text = String::from("name: big\nmanaged_by: fleet\n#");
        text.push_str(&"x".repeat(MAX_BUNDLE_BYTES));
        fs::write(dir.join("big.yaml"), text).unwrap();
        assert!(read_host_dir(&dir).is_err());
        fs::remove_dir_all(dir).unwrap();
    }

    #[test]
    fn rejects_non_regular_yaml_entries() {
        let dir = temp_dir("kinds");
        fs::write(dir.join("ok.yaml"), "name: ok\nmanaged_by: fleet\n").unwrap();
        fs::write(dir.join("ignored.txt"), "x").unwrap();
        fs::create_dir(dir.join("directory.yaml")).unwrap();
        let err = read_host_dir(&dir).unwrap_err();
        assert!(err.contains("not a regular file"), "{err}");
        assert!(err.contains("directory.yaml"), "{err}");
        fs::remove_dir_all(dir).unwrap();
    }

    #[cfg(unix)]
    #[test]
    fn rejects_symlinked_yaml_entries() {
        let dir = temp_dir("symlink");
        let target = dir.parent().unwrap().join(format!(
            "{}-target.yaml",
            dir.file_name().unwrap().to_string_lossy()
        ));
        fs::write(&target, "name: linked\nmanaged_by: fleet\n").unwrap();
        fs::write(
            dir.join("regular.yaml"),
            "name: regular\nmanaged_by: fleet\n",
        )
        .unwrap();
        std::os::unix::fs::symlink(&target, dir.join("linked.yaml")).unwrap();
        let err = read_host_dir(&dir).unwrap_err();
        assert!(err.contains("linked.yaml"), "{err}");
        assert!(err.contains("not a regular file"), "{err}");
        fs::remove_dir_all(&dir).unwrap();
        fs::remove_file(target).unwrap();
    }
}
