package agent

import (
	"context"
	"errors"
	"io"
	"iter"
	"net/netip"
	"slices"
	"strings"
	"testing"

	dockerspec "github.com/moby/docker-image-spec/specs-go/v1"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/image"
	"github.com/moby/moby/api/types/jsonstream"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// fakeDocker records calls and can be told to fail specific ones.
type fakeDocker struct {
	calls []string

	inspect    container.InspectResponse // returned by ContainerInspect
	pullReader io.ReadCloser             // overrides pullStream when set

	pullStream string // body returned by ImagePull
	images     map[string]image.InspectResponse

	failCreate, failStart, failConnect, failStop error

	created     *container.Config
	createdHost *container.HostConfig
	createdNet  *network.NetworkingConfig
	connected   []string
}

func (f *fakeDocker) log(s string) { f.calls = append(f.calls, s) }

func (f *fakeDocker) ContainerInspect(ctx context.Context, id string, _ client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	if f.inspect.ID == "" {
		return client.ContainerInspectResult{}, errors.New("no such container")
	}
	return client.ContainerInspectResult{Container: f.inspect}, nil
}
func (f *fakeDocker) ContainerCreate(ctx context.Context, o client.ContainerCreateOptions) (client.ContainerCreateResult, error) {
	f.log("create " + o.Name)
	if f.failCreate != nil {
		return client.ContainerCreateResult{}, f.failCreate
	}
	f.created, f.createdHost, f.createdNet = o.Config, o.HostConfig, o.NetworkingConfig
	return client.ContainerCreateResult{ID: "new-id"}, nil
}
func (f *fakeDocker) ContainerStart(ctx context.Context, id string, _ client.ContainerStartOptions) (client.ContainerStartResult, error) {
	f.log("start " + id)
	if id == "new-id" && f.failStart != nil {
		return client.ContainerStartResult{}, f.failStart
	}
	return client.ContainerStartResult{}, nil
}
func (f *fakeDocker) ContainerStop(ctx context.Context, id string, _ client.ContainerStopOptions) (client.ContainerStopResult, error) {
	f.log("stop " + id)
	return client.ContainerStopResult{}, f.failStop
}
func (f *fakeDocker) ContainerRemove(ctx context.Context, id string, _ client.ContainerRemoveOptions) (client.ContainerRemoveResult, error) {
	f.log("remove " + id)
	return client.ContainerRemoveResult{}, nil
}
func (f *fakeDocker) ContainerRename(ctx context.Context, id string, o client.ContainerRenameOptions) (client.ContainerRenameResult, error) {
	name := o.NewName
	if strings.Contains(name, "-sddb-old-") {
		name = "<backup>"
	}
	f.log("rename " + id + " " + name)
	return client.ContainerRenameResult{}, nil
}
func (f *fakeDocker) NetworkConnect(ctx context.Context, netID string, o client.NetworkConnectOptions) (client.NetworkConnectResult, error) {
	f.log("connect " + netID)
	f.connected = append(f.connected, netID)
	return client.NetworkConnectResult{}, f.failConnect
}
func (f *fakeDocker) ImagePull(ctx context.Context, ref string, _ client.ImagePullOptions) (client.ImagePullResponse, error) {
	f.log("pull " + ref)
	if f.pullReader != nil {
		return fakePull{f.pullReader}, nil
	}
	return fakePull{io.NopCloser(strings.NewReader(f.pullStream))}, nil
}
func (f *fakeDocker) ImageInspect(ctx context.Context, id string, _ ...client.ImageInspectOption) (client.ImageInspectResult, error) {
	img, ok := f.images[id]
	if !ok {
		return client.ImageInspectResult{}, errors.New("no such image " + id)
	}
	return client.ImageInspectResult{InspectResponse: img}, nil
}

// fakePull is a pull stream; the agent only reads it as an io.Reader.
type fakePull struct{ io.ReadCloser }

func (fakePull) JSONMessages(context.Context) iter.Seq2[jsonstream.Message, error] {
	panic("not used")
}
func (fakePull) Wait(context.Context) error { panic("not used") }

func imgCfg(env, cmd []string) *dockerspec.DockerOCIImageConfig {
	return &dockerspec.DockerOCIImageConfig{ImageConfig: ocispec.ImageConfig{Env: env, Cmd: cmd}}
}

const oldID = "0123456789abcdef0123"

func newFake() *fakeDocker {
	return &fakeDocker{
		pullStream: `{"status":"Pulling"}` + "\n" + `{"status":"Done"}` + "\n",
		images: map[string]image.InspectResponse{
			"sha256:old":         {ID: "sha256:old", Config: imgCfg([]string{"PATH=/old", "APP_VERSION=1"}, []string{"serve"})},
			"example/app:latest": {ID: "sha256:new", Config: imgCfg([]string{"PATH=/new", "APP_VERSION=2"}, []string{"serve", "--fast"})},
		},
	}
}

func runningContainer() container.InspectResponse {
	return container.InspectResponse{
		ID:         oldID,
		Name:       "/app",
		Image:      "sha256:old",
		State:      &container.State{Running: true},
		HostConfig: &container.HostConfig{NetworkMode: "appnet", Binds: []string{"data:/data"}},
		Config: &container.Config{
			Image:    "example/app:latest",
			Hostname: oldID[:12],
			Env:      []string{"PATH=/old", "APP_VERSION=1", "TZ=Europe/London"},
			Cmd:      []string{"serve"},
		},
		Mounts: []container.MountPoint{
			{Type: mount.TypeVolume, Name: "data", Destination: "/data", RW: true},
			{Type: mount.TypeVolume, Name: "anon123", Destination: "/var/lib/app", RW: true},
		},
		NetworkSettings: &container.NetworkSettings{Networks: map[string]*network.EndpointSettings{
			"appnet":  {Aliases: []string{"app", oldID[:12]}, IPAddress: netip.MustParseAddr("172.18.0.5"), EndpointID: "ep1"},
			"monitor": {Aliases: []string{"app"}},
		}},
	}
}

func TestUpgradeCLISuccess(t *testing.T) {
	f := newFake()
	if err := upgradeCLI(context.Background(), f, runningContainer()); err != nil {
		t.Fatalf("upgrade: %v", err)
	}

	want := []string{
		"pull example/app:latest",
		"rename " + oldID + " <backup>",
		"create app",
		"connect monitor",
		"stop " + oldID,
		"start new-id",
		"remove " + oldID,
	}
	if !slices.Equal(f.calls, want) {
		t.Fatalf("calls:\n got %q\nwant %q", f.calls, want)
	}

	// Image defaults refreshed, user settings kept.
	if got, want := f.created.Env, []string{"TZ=Europe/London"}; !slices.Equal(got, want) {
		t.Errorf("env = %q, want %q (old image defaults dropped so the new ones apply)", got, want)
	}
	if f.created.Cmd != nil {
		t.Errorf("cmd = %q, want nil so the new image's default is used", f.created.Cmd)
	}
	if f.created.Hostname != "" {
		t.Errorf("hostname = %q, want default for the new container", f.created.Hostname)
	}
	// Anonymous volume reattached, named volume untouched.
	if got, want := f.createdHost.Binds, []string{"data:/data", "anon123:/var/lib/app"}; !slices.Equal(got, want) {
		t.Errorf("binds = %q, want %q", got, want)
	}
	// Only the primary network at create time, with runtime fields stripped.
	ep := f.createdNet.EndpointsConfig["appnet"]
	if len(f.createdNet.EndpointsConfig) != 1 || ep == nil {
		t.Fatalf("create networks = %v, want only appnet", f.createdNet.EndpointsConfig)
	}
	if ep.IPAddress.IsValid() || ep.EndpointID != "" || !slices.Equal(ep.Aliases, []string{"app"}) {
		t.Errorf("endpoint not cleaned: %+v", ep)
	}
}

func TestUpgradeCLIRollsBackWhenCreateFails(t *testing.T) {
	f := newFake()
	f.failCreate = errors.New("network appnet not found")

	err := upgradeCLI(context.Background(), f, runningContainer())
	if err == nil || !strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("err = %v, want rolled back error", err)
	}
	want := []string{
		"pull example/app:latest",
		"rename " + oldID + " <backup>",
		"create app",
		"rename " + oldID + " app", // old container was never stopped, so nothing to restart
	}
	if !slices.Equal(f.calls, want) {
		t.Fatalf("calls:\n got %q\nwant %q", f.calls, want)
	}
}

func TestUpgradeCLIRollsBackWhenStartFails(t *testing.T) {
	f := newFake()
	f.failStart = errors.New("port is already allocated")

	err := upgradeCLI(context.Background(), f, runningContainer())
	if err == nil || !strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("err = %v, want rolled back error", err)
	}
	want := []string{
		"pull example/app:latest",
		"rename " + oldID + " <backup>",
		"create app",
		"connect monitor",
		"stop " + oldID,
		"start new-id",
		"remove new-id",
		"rename " + oldID + " app",
		"start " + oldID,
	}
	if !slices.Equal(f.calls, want) {
		t.Fatalf("calls:\n got %q\nwant %q", f.calls, want)
	}
	for _, c := range f.calls {
		if c == "remove "+oldID {
			t.Fatal("old container must never be removed when the upgrade fails")
		}
	}
}

func TestUpgradeCLIStopsBeforeTouchingContainerWhenPullFails(t *testing.T) {
	f := newFake()
	f.pullStream = `{"status":"Pulling"}` + "\n" + `{"errorDetail":{"message":"unauthorized"},"error":"unauthorized"}` + "\n"

	err := upgradeCLI(context.Background(), f, runningContainer())
	if err == nil || !strings.Contains(err.Error(), "unauthorized") {
		t.Fatalf("err = %v, want the in-stream pull error", err)
	}
	if !slices.Equal(f.calls, []string{"pull example/app:latest"}) {
		t.Fatalf("container was touched after a failed pull: %q", f.calls)
	}
}

func TestUpgradeCLINoOpWhenAlreadyLatest(t *testing.T) {
	f := newFake()
	f.images["example/app:latest"] = f.images["sha256:old"]

	if err := upgradeCLI(context.Background(), f, runningContainer()); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.calls, []string{"pull example/app:latest"}) {
		t.Fatalf("container recreated although the image did not change: %q", f.calls)
	}
}

func TestUpgradeCLIRejectsImageIDContainers(t *testing.T) {
	f := newFake()
	c := runningContainer()
	c.Config.Image = "sha256:a599effa096f5affa2fcd776c735a7d9e9a56d8c52d9dab1004af95618f474bc"

	err := upgradeCLI(context.Background(), f, c)
	if err == nil || !strings.Contains(err.Error(), "image ID") {
		t.Fatalf("err = %v, want image ID error", err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("unexpected calls %q", f.calls)
	}
}

func TestUpgradeCLIStoppedContainerIsNotStarted(t *testing.T) {
	f := newFake()
	c := runningContainer()
	c.State.Running = false

	if err := upgradeCLI(context.Background(), f, c); err != nil {
		t.Fatal(err)
	}
	for _, call := range f.calls {
		if strings.HasPrefix(call, "start ") || strings.HasPrefix(call, "stop ") {
			t.Fatalf("stopped container must stay stopped, got %q", f.calls)
		}
	}
}

func TestSplitNetworksHostMode(t *testing.T) {
	c := runningContainer()
	c.HostConfig.NetworkMode = "host"
	nc, extra := splitNetworks(c, oldID[:12])
	if nc != nil || extra != nil {
		t.Fatalf("host networking should not pass endpoint config, got %v %v", nc, extra)
	}
}

func TestDrainPullStream(t *testing.T) {
	if err := drainPullStream(strings.NewReader(`{"status":"a"}{"status":"b"}`)); err != nil {
		t.Fatalf("clean stream: %v", err)
	}
	if err := drainPullStream(strings.NewReader(`{"status":"a"}{"error":"manifest unknown"}`)); err == nil || err.Error() != "manifest unknown" {
		t.Fatalf("err = %v, want manifest unknown", err)
	}
}
