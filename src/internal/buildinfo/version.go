package buildinfo

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

func Format(info *debug.BuildInfo) string {
	revision, stamp, goVersion := "unknown", "unknown", runtime.Version()
	modified := false
	if info != nil {
		if info.GoVersion != "" {
			goVersion = info.GoVersion
		}
		for _, setting := range info.Settings {
			switch setting.Key {
			case "vcs.revision":
				if setting.Value != "" {
					revision = setting.Value
				}
			case "vcs.time":
				if setting.Value != "" {
					stamp = setting.Value
				}
			case "vcs.modified":
				modified = setting.Value == "true"
			}
		}
	}
	if modified {
		revision += "-dirty"
	}
	return fmt.Sprintf("revision: %s\ntime: %s\ngo: %s\n", revision, stamp, goVersion)
}

func Current() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return Format(nil)
	}
	return Format(info)
}
