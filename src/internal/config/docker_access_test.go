package config

import "testing"

func TestProfileDockerAccessModeExplicitRootless(t *testing.T) {
	t.Parallel()

	profile := Profile{
		Docker: DockerProfile{
			AccessMode: DockerAccessModeRootless,
			Volumes:    []string{"/run/user/1001/docker.sock:/var/run/docker.sock"},
			Env:        map[string]string{"DOCKER_HOST": "unix:///var/run/docker.sock"},
		},
	}

	if got := profile.DockerAccessMode(); got != DockerAccessModeRootless {
		t.Fatalf("expected %q, got %q", DockerAccessModeRootless, got)
	}
}

func TestProfileDockerAccessModeExplicitHostSocket(t *testing.T) {
	t.Parallel()

	profile := Profile{
		Docker: DockerProfile{
			AccessMode: DockerAccessModeHostSocket,
			Volumes:    []string{"/var/run/docker.sock:/var/run/docker.sock"},
		},
	}

	if got := profile.DockerAccessMode(); got != DockerAccessModeHostSocket {
		t.Fatalf("expected %q, got %q", DockerAccessModeHostSocket, got)
	}
}

func TestProfileDockerAccessModeInfersLegacyHostSocket(t *testing.T) {
	t.Parallel()

	profile := Profile{
		Docker: DockerProfile{
			Volumes: []string{"/var/run/docker.sock:/var/run/docker.sock"},
		},
	}

	if got := profile.DockerAccessMode(); got != DockerAccessModeHostSocket {
		t.Fatalf("expected inferred %q, got %q", DockerAccessModeHostSocket, got)
	}
	if !profile.HasHostDockerSocket() {
		t.Fatal("expected host docker socket to be detected")
	}
}

func TestProfileDockerAccessModeInfersLegacyRootless(t *testing.T) {
	t.Parallel()

	profile := Profile{
		Docker: DockerProfile{
			Volumes: []string{"/run/user/1001/docker.sock:/var/run/docker.sock"},
			Env:     map[string]string{"DOCKER_HOST": "unix:///var/run/docker.sock"},
		},
	}

	if got := profile.DockerAccessMode(); got != DockerAccessModeRootless {
		t.Fatalf("expected inferred %q, got %q", DockerAccessModeRootless, got)
	}
	if !profile.HasRootlessDockerSocket() {
		t.Fatal("expected rootless docker socket to be detected")
	}
}

func TestProfileDockerAccessModeReturnsUnknownForAmbiguousConfig(t *testing.T) {
	t.Parallel()

	profile := Profile{
		Docker: DockerProfile{
			Volumes: []string{
				"/var/run/docker.sock:/var/run/docker.sock",
				"/run/user/1001/docker.sock:/var/run/docker.sock",
			},
			Env: map[string]string{"DOCKER_HOST": "unix:///var/run/docker.sock"},
		},
	}

	if got := profile.DockerAccessMode(); got != "" {
		t.Fatalf("expected unknown access mode, got %q", got)
	}
}
