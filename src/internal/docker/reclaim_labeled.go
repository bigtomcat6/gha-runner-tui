package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// LabeledReclaimAge keeps resources that a job may still be setting up.
const LabeledReclaimAge = time.Hour

const (
	labeledVolumeFormat    = `{{json .}}`
	labeledContainerFormat = `{"id":{{json .Id}},"name":{{json .Name}},"image":{{json .Image}},"status":{{json .State.Status}},"labels":{{json .Config.Labels}},"mounts":{{json .Mounts}}}`
	labeledImageFormat     = `{"id":{{json .Id}},"created":{{json .Created}},"tagged":{{json .Metadata.LastTagTime}},"labels":{{json .Config.Labels}}}`
)

type labeledVolume struct {
	Name       string
	CreatedAt  time.Time
	Mountpoint string
	Labels     map[string]string
}

type labeledImage struct {
	ID      string            `json:"id"`
	Created time.Time         `json:"created"`
	Tagged  time.Time         `json:"tagged"`
	Labels  map[string]string `json:"labels"`
}

// A fully cached build keeps the old creation time; only its tag time is new.
func (i labeledImage) lastBuilt() time.Time {
	if i.Tagged.After(i.Created) {
		return i.Tagged
	}
	return i.Created
}

type containerUse struct {
	ID     string            `json:"id"`
	Name   string            `json:"name"`
	Image  string            `json:"image"`
	Status string            `json:"status"`
	Labels map[string]string `json:"labels"`
	Mounts []struct {
		Type   string
		Name   string
		Source string
	} `json:"mounts"`
}

// ReclaimLabeled removes leftovers of killed jobs that carry an opt-in label:
//
//   - a volume whose label value is its own name, older than LabeledReclaimAge,
//     that no container exposes. Stopped containers named <volume>-* with the
//     same label value are removed with it; any other reference keeps it.
//   - a tagged image with a non-empty label value, built and tagged longer than
//     LabeledReclaimAge ago, that no container uses.
//
// It never forces a removal. Whatever it cannot verify, it keeps. It returns
// what it removed, even when some steps failed.
func (c Client) ReclaimLabeled(ctx context.Context, label string, now time.Time) ([]string, error) {
	if label == "" {
		return nil, errors.New("reclaim label is empty")
	}
	volumes, volumeErr := c.reclaimLabeledVolumes(ctx, label, now)
	images, imageErr := c.reclaimLabeledImages(ctx, label, now)
	return append(volumes, images...), errors.Join(volumeErr, imageErr)
}

func (c Client) reclaimLabeledVolumes(ctx context.Context, label string, now time.Time) ([]string, error) {
	out, err := c.run(ctx, "volume", "ls", "--quiet", "--filter", "label="+label)
	if err != nil {
		return nil, fmt.Errorf("list labeled volumes: %w", err)
	}
	var removed []string
	var errs []error
	for _, name := range strings.Fields(string(out)) {
		var volume labeledVolume
		if err := c.inspectInto(ctx, &volume, "volume", "inspect", "--format", labeledVolumeFormat, name); err != nil {
			errs = append(errs, fmt.Errorf("inspect labeled volume: %w", err))
			continue
		}
		if volume.Name != name || volume.Labels[label] != name || !leftoverIsOld(volume.CreatedAt, now) {
			continue
		}
		// Read right before removal: Docker does not protect a volume whose
		// files another container reaches through a bind mount.
		containers, err := c.containerUses(ctx)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		stopped, safe := volumeLeftovers(volume, label, containers)
		if !safe {
			continue
		}
		if err := c.removeVolume(ctx, name, stopped); err != nil {
			errs = append(errs, err)
			continue
		}
		removed = append(removed, "volume "+name)
	}
	return removed, errors.Join(errs...)
}

func (c Client) removeVolume(ctx context.Context, name string, stopped []string) error {
	for _, id := range stopped {
		if _, err := c.run(ctx, "rm", id); err != nil {
			return fmt.Errorf("remove leftover container: %w", err)
		}
	}
	if _, err := c.run(ctx, "volume", "rm", name); err != nil {
		return fmt.Errorf("remove labeled volume: %w", err)
	}
	return nil
}

func (c Client) reclaimLabeledImages(ctx context.Context, label string, now time.Time) ([]string, error) {
	out, err := c.run(ctx, "image", "ls", "--filter", "label="+label, "--format", "{{.Repository}}:{{.Tag}}")
	if err != nil {
		return nil, fmt.Errorf("list labeled images: %w", err)
	}
	var removed []string
	var errs []error
	for _, tag := range strings.Fields(string(out)) {
		if strings.Contains(tag, "<none>") {
			continue // untagged images are left to the dangling prune
		}
		var image labeledImage
		if err := c.inspectInto(ctx, &image, "image", "inspect", "--format", labeledImageFormat, tag); err != nil {
			errs = append(errs, fmt.Errorf("inspect labeled image: %w", err))
			continue
		}
		if image.ID == "" || image.Labels[label] == "" || !leftoverIsOld(image.lastBuilt(), now) {
			continue
		}
		containers, err := c.containerUses(ctx)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		used := false
		for _, container := range containers {
			used = used || container.Image == image.ID
		}
		if used {
			continue
		}
		if _, err := c.run(ctx, "image", "rm", "--no-prune", tag); err != nil {
			errs = append(errs, fmt.Errorf("remove labeled image: %w", err))
			continue
		}
		removed = append(removed, "image "+tag)
	}
	return removed, errors.Join(errs...)
}

func (c Client) inspectInto(ctx context.Context, value any, args ...string) error {
	out, err := c.run(ctx, args...)
	if err != nil {
		return err
	}
	if json.Unmarshal(out, value) != nil {
		return errInspectMalformed
	}
	return nil
}

// containerUses reports every container or fails: a partial view must never
// authorize a removal. Environment values are deliberately not requested.
func (c Client) containerUses(ctx context.Context) ([]containerUse, error) {
	out, err := c.run(ctx, "ps", "--all", "--quiet", "--no-trunc")
	if err != nil {
		return nil, fmt.Errorf("list container references: %w", err)
	}
	ids := strings.Fields(string(out))
	if len(ids) == 0 {
		return nil, nil
	}
	out, err = c.run(ctx, append([]string{"container", "inspect", "--format", labeledContainerFormat}, ids...)...)
	if err != nil {
		return nil, fmt.Errorf("inspect container references: %w", err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != len(ids) {
		return nil, errors.New("container reference count mismatch")
	}
	uses := make([]containerUse, len(lines))
	for i, line := range lines {
		if json.Unmarshal([]byte(line), &uses[i]) != nil || uses[i].ID == "" || uses[i].Status == "" {
			return nil, errInspectMalformed
		}
	}
	return uses, nil
}

// volumeLeftovers returns the stopped containers to remove with the volume, or
// safe=false when anything else can reach the volume's files.
func volumeLeftovers(volume labeledVolume, label string, containers []containerUse) (stopped []string, safe bool) {
	for _, container := range containers {
		mounted := false
		for _, mount := range container.Mounts {
			switch mount.Type {
			case "volume":
				mounted = mounted || mount.Name == volume.Name
			case "bind":
				if pathsOverlap(mount.Source, volume.Mountpoint) {
					return nil, false
				}
			}
		}
		if !mounted {
			continue
		}
		// The label alone is no proof: containers inherit their image's labels.
		if container.Labels[label] != volume.Name || !strings.HasPrefix(container.Name, "/"+volume.Name+"-") {
			return nil, false
		}
		switch container.Status {
		case "created", "exited", "dead":
			stopped = append(stopped, container.ID)
		default:
			return nil, false
		}
	}
	return stopped, true
}

// pathsOverlap compares literal daemon paths and treats anything it cannot
// compare as overlapping.
func pathsOverlap(a, b string) bool {
	for _, p := range []string{a, b} {
		if !filepath.IsAbs(p) || strings.Contains(p+"/", "/../") {
			return true
		}
	}
	within := func(child, parent string) bool {
		rel, err := filepath.Rel(parent, child)
		return err != nil || (rel != ".." && !strings.HasPrefix(rel, "../"))
	}
	return within(a, b) || within(b, a)
}

func leftoverIsOld(created, now time.Time) bool {
	return !created.IsZero() && now.Sub(created) > LabeledReclaimAge
}
