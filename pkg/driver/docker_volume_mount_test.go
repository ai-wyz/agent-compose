package driver

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	containerapi "github.com/docker/docker/api/types/container"
	mountapi "github.com/docker/docker/api/types/mount"
)

// The published Compose deployment runs the daemon in a container with the
// host's data directory (here /engine/data) bound at DATA_ROOT, SANDBOX_ROOT
// under it, and DOCKER_HOST_SANDBOX_ROOT set because the daemon's paths are not
// Engine host paths. A local project volume lives at DATA_ROOT/volumes, beside
// SANDBOX_ROOT, and a bind volume can point anywhere in the data mount.
func TestDockerVolumesBesideSandboxRootMapThroughTheDaemonDataMount(t *testing.T) {
	for _, test := range []struct {
		name, mountType, relativeSource string
	}{
		{name: "local project volume", mountType: "volume", relativeSource: "volumes/local/volume-1/data"},
		{name: "bind volume", mountType: "bind", relativeSource: "projects/reports/outputs"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dataRoot := t.TempDir()
			config := testRuntimeMountConfig()
			config.SandboxRoot = filepath.Join(dataRoot, "sandboxes")
			config.DockerHostSandboxRoot = "/engine/data/sandboxes"
			sandbox := testRuntimeMountSandbox(filepath.Join(config.SandboxRoot, "sandbox-1"))
			sandbox.VolumeMounts = []SandboxVolumeMount{{Type: test.mountType, Source: "outputs", Target: "/outputs", HostPath: filepath.Join(dataRoot, test.relativeSource)}}
			if _, err := prepareRuntimeMountManifest(config, sandbox, RuntimeDriverDocker); err != nil {
				t.Fatal(err)
			}
			self := containerapi.InspectResponse{Mounts: []containerapi.MountPoint{{Type: mountapi.TypeBind, Source: "/engine/data", Destination: dataRoot}}}
			runtime := &dockerRuntime{config: config, workspaceProcess: testContainerizedDockerWorkspaceProcess}

			mounts, err := runtime.dockerRuntimeMounts(context.Background(), workspaceDockerClient(t, "1.44", self), sandbox)
			if err != nil {
				t.Fatalf("%s outside SANDBOX_ROOT was rejected: %v", test.name, err)
			}
			found := false
			for _, mount := range mounts {
				if mount.Target == "/outputs" {
					found = true
					if want := "/engine/data/" + test.relativeSource; mount.Source != want {
						t.Fatalf("volume source = %q, want %q", mount.Source, want)
					}
					continue
				}
				// Sandbox-owned paths still use the explicit override.
				if !strings.HasPrefix(mount.Source, "/engine/data/sandboxes/sandbox-1/") {
					t.Fatalf("owned mount did not use DOCKER_HOST_SANDBOX_ROOT: %+v", mount)
				}
			}
			if !found {
				t.Fatalf("volume mount missing: %+v", mounts)
			}
		})
	}
}

func TestDockerVolumeOutsideEveryDaemonMountIsRejectedUnderOverride(t *testing.T) {
	dataRoot := t.TempDir()
	config := testRuntimeMountConfig()
	config.SandboxRoot = filepath.Join(dataRoot, "sandboxes")
	config.DockerHostSandboxRoot = "/engine/data/sandboxes"
	sandbox := testRuntimeMountSandbox(filepath.Join(config.SandboxRoot, "sandbox-1"))
	sandbox.VolumeMounts = []SandboxVolumeMount{{Type: "volume", Source: "outputs", Target: "/outputs", HostPath: filepath.Join(dataRoot, "volumes", "local", "volume-1", "data")}}
	if _, err := prepareRuntimeMountManifest(config, sandbox, RuntimeDriverDocker); err != nil {
		t.Fatal(err)
	}
	// Only SANDBOX_ROOT is shared, so the volume exists solely in the daemon
	// container's writable layer: mounting its daemon path would expose whatever
	// the Engine host has at that path instead.
	self := containerapi.InspectResponse{Mounts: []containerapi.MountPoint{{Type: mountapi.TypeBind, Source: "/engine/data/sandboxes", Destination: config.SandboxRoot}}}
	runtime := &dockerRuntime{config: config, workspaceProcess: testContainerizedDockerWorkspaceProcess}
	_, err := runtime.dockerRuntimeMounts(context.Background(), workspaceDockerClient(t, "1.44", self), sandbox)
	if err == nil || !strings.Contains(err.Error(), "writable-layer") {
		t.Fatalf("unshared volume error = %v; want a writable-layer rejection", err)
	}
}
