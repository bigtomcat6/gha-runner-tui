package systemd

import (
	"strings"
	"testing"
)

func TestParseUnitShowUsesEffectiveExecStartAndDropIns(t *testing.T) {
	info, err := ParseUnitShow("LoadState=loaded\nActiveState=activating\nFragmentPath=/etc/systemd/system/gha-a.service\nDropInPaths=/etc/systemd/system/gha-a.service.d/10-x.conf /etc/systemd/system/gha-a.service.d/zz-exec.conf\nExecStart={ path=/bin/loop ; argv[]=/bin/loop --config /etc/gha-runner-tui/profiles-live/a.yaml ; ignore_errors=no ; start_time=[n/a] ; stop_time=[n/a] ; pid=0 ; code=(null) ; status=0/0 }\n")
	if err != nil {
		t.Fatal(err)
	}
	if info.LoadState != "loaded" || info.ActiveState != "activating" || len(info.DropInPaths) != 2 {
		t.Fatalf("info=%+v", info)
	}
	if path, err := info.ConfigPath(); err != nil || path != "/etc/gha-runner-tui/profiles-live/a.yaml" {
		t.Fatalf("config=%q err=%v", path, err)
	}
}

func TestParseUnitShowRejectsAmbiguousConfig(t *testing.T) {
	for _, tc := range []struct{ name, text string }{
		{"none", "LoadState=loaded\nExecStart={ path=/bin/loop ; argv[]=/bin/loop ; ignore_errors=no }\n"},
		{"duplicate", "LoadState=loaded\nExecStart={ path=/bin/loop ; argv[]=/bin/loop --config /a --config /b ; ignore_errors=no }\n"},
		{"multiple-exec", "LoadState=loaded\nExecStart={ path=/bin/loop ; argv[]=/bin/loop --config /a ; ignore_errors=no }\nExecStart={ path=/bin/loop ; argv[]=/bin/loop --config /b ; ignore_errors=no }\n"},
		{"equals", "LoadState=loaded\nExecStart={ path=/bin/loop ; argv[]=/bin/loop --config=/a ; ignore_errors=no }\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info, err := ParseUnitShow(tc.text)
			if err != nil {
				if tc.name == "multiple-exec" {
					return
				}
				t.Fatal(err)
			}
			path, err := info.ConfigPath()
			if tc.name == "equals" {
				if err != nil || path != "/a" {
					t.Fatalf("equals config=%q err=%v", path, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "config") {
				t.Fatalf("accepted ambiguous: %q %v", path, err)
			}
		})
	}
}
