package app

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"gha-runner-tui/internal/config"
)

// writeFileAtomic writes data beside the target, fsyncs the temporary file,
// renames it over the target, and fsyncs the directory. Existing files keep
// their permission bits and, when the process may do so, their owner.
func writeFileAtomic(dir, name string, data []byte, perm os.FileMode) error {
	target := filepath.Join(dir, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	existing := os.FileMode(0)
	var stat syscall.Stat_t
	haveOwner := false
	if info, err := os.Lstat(target); err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("refusing to replace non-regular file %q", target)
		}
		existing = info.Mode().Perm()
		if raw, ok := info.Sys().(*syscall.Stat_t); ok {
			stat = *raw
			haveOwner = true
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if existing != 0 {
		perm = existing
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if haveOwner {
		// Best effort: an unprivileged process cannot change a foreign owner,
		// but then it also cannot have opened the file for replacement.
		_ = os.Chown(tmpName, int(stat.Uid), int(stat.Gid))
	}
	if err := os.Rename(tmpName, target); err != nil {
		return err
	}
	return syncDir(dir)
}

func backupDir(cfg config.GlobalConfig) string {
	return filepath.Join(filepath.Dir(cfg.Paths.StateDir), "profile-backups")
}

func backupProfile(cfg config.GlobalConfig, name string, raw []byte) (string, error) {
	dir := backupDir(cfg)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	timestamp := time.Now().UTC().Format("20060102T150405.000000000Z")
	if err := writeFileAtomic(dir, name+"-"+timestamp+".yaml", raw, 0o600); err != nil {
		return "", err
	}
	if err := pruneBackups(dir, name, 5); err != nil {
		return "", err
	}
	return filepath.Join(dir, name+"-"+timestamp+".yaml"), nil
}

func pruneBackups(dir, name string, keep int) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var backups []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		base := entry.Name()
		if !strings.HasPrefix(base, name+"-") || !strings.HasSuffix(base, ".yaml") {
			continue
		}
		rest := strings.TrimSuffix(strings.TrimPrefix(base, name+"-"), ".yaml")
		if _, err := time.Parse("20060102T150405.000000000Z", rest); err != nil {
			continue
		}
		backups = append(backups, filepath.Join(dir, base))
	}
	sort.Strings(backups)
	for len(backups) > keep {
		if err := os.Remove(backups[0]); err != nil {
			return err
		}
		backups = backups[1:]
	}
	return nil
}

func fileSHA(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return shaOfBytes(data), nil
}

func fileExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func copyFile(src, dst string, perm os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	info, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	syncErr := out.Sync()
	closeErr := out.Close()
	if raw, ok := info.Sys().(*syscall.Stat_t); ok {
		_ = os.Chown(dst, int(raw.Uid), int(raw.Gid))
	}
	return errors.Join(copyErr, syncErr, closeErr)
}

// ignoreTerminalHangup keeps an execution batch together when an SSH client
// disconnects. Errors writing the JSON result are handled by the caller.
func ignoreTerminalHangup() {
	signal.Ignore(syscall.SIGHUP, syscall.SIGPIPE)
}
