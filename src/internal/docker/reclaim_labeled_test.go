package docker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

const (
	testReclaimLabel = "com.example.ci"
	testVolume       = "ci-0123"
	testMountpoint   = "/data/docker/volumes/ci-0123/_data"
)

var reclaimNow = time.Date(2026, 11, 22, 8, 0, 0, 0, time.UTC)

type reclaimCase struct {
	name       string
	owner      string
	age        time.Duration
	tagAge     time.Duration
	containers []string
	psErr      error
	inspectErr error
	removed    bool
	wantErr    bool
}

func testContainer(status, owner, image, mount string) string {
	return fmt.Sprintf(`{"id":"c","name":"/ci-0123-stage","image":%q,"status":%q,"labels":{%q:%q},"mounts":[%s]}`, image, status, testReclaimLabel, owner, mount)
}

func testVolumeMount(name string) string {
	return fmt.Sprintf(`{"Type":"volume","Name":%q,"Source":"/elsewhere"}`, name)
}

func testBindMount(source string) string {
	return fmt.Sprintf(`{"Type":"bind","Source":%q}`, source)
}

func (tc reclaimCase) run(t *testing.T, kind string) ([]string, []string, error) {
	t.Helper()
	created := reclaimNow.Add(-tc.age).Format(time.RFC3339)
	var removals []string
	client := NewClient(dockerRunnerFunc(func(_ context.Context, _ string, args ...string) ([]byte, error) {
		call := strings.Join(args[2:], " ")
		switch {
		case strings.Contains(call, "--force"):
			t.Errorf("forced call: %q", call)
		case strings.HasPrefix(call, "volume ls"):
			if kind == "volume" {
				return []byte(testVolume + "\n"), nil
			}
			return nil, nil
		case strings.HasPrefix(call, "image ls"):
			if kind == "image" {
				return []byte("ci-worker-0123:latest\n"), nil
			}
			return nil, nil
		case strings.HasPrefix(call, "volume inspect"):
			return []byte(fmt.Sprintf(`{"Name":%q,"CreatedAt":%q,"Mountpoint":%q,"Labels":{%q:%q}}`, testVolume, created, testMountpoint, testReclaimLabel, tc.owner)), nil
		case strings.HasPrefix(call, "image inspect"):
			tagged := "0001-01-01T00:00:00Z"
			if tc.tagAge != 0 {
				tagged = reclaimNow.Add(-tc.tagAge).Format(time.RFC3339)
			}
			return []byte(fmt.Sprintf(`{"id":"sha256:worker","created":%q,"tagged":%q,"labels":{%q:%q}}`, created, tagged, testReclaimLabel, tc.owner)), nil
		case strings.HasPrefix(call, "ps "):
			return []byte(strings.Repeat("c\n", len(tc.containers))), tc.psErr
		case strings.HasPrefix(call, "container inspect"):
			return []byte(strings.Join(tc.containers, "\n")), tc.inspectErr
		case strings.HasPrefix(call, "rm "), strings.HasPrefix(call, "volume rm "), strings.HasPrefix(call, "image rm "):
			removals = append(removals, call)
			return nil, nil
		}
		return nil, fmt.Errorf("unexpected docker call %q", call)
	}))
	removed, err := client.ReclaimLabeled(context.Background(), testReclaimLabel, reclaimNow)
	return removed, removals, err
}

func TestReclaimLabeledVolumes(t *testing.T) {
	old := 2 * time.Hour
	mounted := testVolumeMount(testVolume)
	for _, tc := range []reclaimCase{
		{name: "orphan", owner: testVolume, age: old, removed: true},
		{name: "unrelated container", owner: testVolume, age: old, removed: true,
			containers: []string{testContainer("running", "", "x", testBindMount("/var/run/docker.sock"))}},
		{name: "sibling path", owner: testVolume, age: old, removed: true,
			containers: []string{testContainer("running", "", "x", testBindMount("/data/docker/volumes/ci-0123-copy"))}},
		{name: "stopped owned helper", owner: testVolume, age: old, removed: true,
			containers: []string{testContainer("created", testVolume, "x", mounted)}},
		{name: "young", owner: testVolume, age: time.Minute},
		{name: "exactly at the age limit", owner: testVolume, age: LabeledReclaimAge},
		{name: "label is not the volume name", owner: "someone-else", age: old},
		{name: "running owned worker", owner: testVolume, age: old,
			containers: []string{testContainer("running", testVolume, "x", mounted)}},
		{name: "paused owned worker", owner: testVolume, age: old,
			containers: []string{testContainer("paused", testVolume, "x", mounted)}},
		{name: "stopped foreign container", owner: testVolume, age: old,
			containers: []string{testContainer("exited", "someone-else", "x", mounted)}},
		{name: "stopped foreign container with inherited label", owner: testVolume, age: old,
			containers: []string{strings.Replace(testContainer("exited", testVolume, "x", mounted), "/ci-0123-stage", "/debug-shell", 1)}},
		{name: "bind of the files", owner: testVolume, age: old,
			containers: []string{testContainer("running", "", "x", testBindMount(testMountpoint+"/target"))}},
		{name: "bind of an ancestor", owner: testVolume, age: old,
			containers: []string{testContainer("exited", "", "x", testBindMount("/data/docker"))}},
		{name: "bind of root", owner: testVolume, age: old,
			containers: []string{testContainer("running", "", "x", testBindMount("/"))}},
		{name: "incomparable bind", owner: testVolume, age: old,
			containers: []string{testContainer("running", "", "x", testBindMount("/srv/../data"))}},
		{name: "list fails", owner: testVolume, age: old, psErr: errors.New("down"), wantErr: true},
		{name: "inspect fails", owner: testVolume, age: old, inspectErr: errors.New("gone"), wantErr: true,
			containers: []string{testContainer("running", "", "x", "")}},
		{name: "inspect is malformed", owner: testVolume, age: old, wantErr: true,
			containers: []string{"not json"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			removed, calls, err := tc.run(t, "volume")
			if (err != nil) != tc.wantErr || (len(removed) == 1) != tc.removed {
				t.Fatalf("removed=%v err=%v", removed, err)
			}
			if !tc.removed && len(calls) != 0 {
				t.Fatalf("kept volume but ran %v", calls)
			}
			if tc.removed && calls[len(calls)-1] != "volume rm "+testVolume {
				t.Fatalf("calls=%v", calls)
			}
		})
	}
}

func TestReclaimLabeledImages(t *testing.T) {
	old := 2 * time.Hour
	for _, tc := range []reclaimCase{
		{name: "orphan", owner: testVolume, age: old, removed: true},
		{name: "young", owner: testVolume, age: time.Minute},
		{name: "cached build tagged just now", owner: testVolume, age: 72 * time.Hour, tagAge: time.Minute},
		{name: "empty label", age: old},
		{name: "used by a stopped container", owner: testVolume, age: old,
			containers: []string{testContainer("exited", "", "sha256:worker", "")}},
		{name: "list fails", owner: testVolume, age: old, psErr: errors.New("down"), wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			removed, calls, err := tc.run(t, "image")
			if (err != nil) != tc.wantErr || (len(removed) == 1) != tc.removed {
				t.Fatalf("removed=%v err=%v", removed, err)
			}
			if !tc.removed && len(calls) != 0 {
				t.Fatalf("kept image but ran %v", calls)
			}
			if tc.removed && calls[0] != "image rm --no-prune ci-worker-0123:latest" {
				t.Fatalf("calls=%v", calls)
			}
		})
	}
}

func TestReclaimLabeledRejectsEmptyLabel(t *testing.T) {
	client := NewClient(dockerRunnerFunc(func(context.Context, string, ...string) ([]byte, error) {
		t.Fatal("docker must not be called without a label")
		return nil, nil
	}))
	if _, err := client.ReclaimLabeled(context.Background(), "", reclaimNow); err == nil {
		t.Fatal("empty label accepted")
	}
}
