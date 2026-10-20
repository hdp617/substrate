//go:build linux

// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"go.ate.dev/substrate/cmd/ateworker-microvm/internal/kata"
	"go.ate.dev/substrate/internal/hardware"
	"go.ate.dev/substrate/internal/ocispec"
	"go.ate.dev/substrate/internal/proto/ateworkerpb"
	"go.ate.dev/substrate/internal/roottest"
	"k8s.io/utils/ptr"
)

// fakeCopier records the files copied into a guest. A non-nil block channel
// holds every copy until it is closed or the copy's context ends, and a
// non-nil entered channel receives each copy as it starts. agentTemps makes
// it write each file the way the kata agent does: through a temp file named
// like it with its extension replaced by ".tmp".
type fakeCopier struct {
	mu         sync.Mutex
	files      map[string]string
	copies     []string
	attempts   int
	block      chan struct{}
	entered    chan string
	err        error
	agentTemps bool
	// afterCopy, when set, runs after each copy that succeeds.
	afterCopy func()
}

func newFakeCopier() *fakeCopier { return &fakeCopier{files: map[string]string{}} }

func (c *fakeCopier) CopyFile(ctx context.Context, path string, data []byte, _ fs.FileMode) error {
	c.mu.Lock()
	c.attempts++
	c.mu.Unlock()
	if c.entered != nil {
		select {
		case c.entered <- path:
		default:
		}
	}
	if c.block != nil {
		select {
		case <-c.block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return c.err
	}
	if c.agentTemps {
		delete(c.files, strings.TrimSuffix(path, filepath.Ext(path))+".tmp")
	}
	c.files[path] = string(data)
	c.copies = append(c.copies, path)
	if c.afterCopy != nil {
		c.afterCopy()
	}
	return nil
}

func (c *fakeCopier) setErr(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.err = err
}

func (c *fakeCopier) attemptCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.attempts
}

func (c *fakeCopier) has(path string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.files[path]
	return ok
}

func (c *fakeCopier) copied() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.copies)
}

func (c *fakeCopier) file(path string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.files[path]
}

func writeFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

// replaceFile writes path the way atelet does: a temp file renamed over it.
func replaceFile(t *testing.T, path, data string) {
	t.Helper()
	tmp := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".tmp")
	writeFile(t, tmp, data)
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

func guestFile(rel string) string {
	return ocispec.GuestSharedDir + "/" + ocispec.ShareSystemInfoCopy + "/" + rel
}

func TestReadSystemInfoFiles(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "meta", "uid"), "actor-1")
	writeFile(t, filepath.Join(root, "bundle", "certs", "ca.pem"), "PEM")
	// An in-flight atelet write, a symlink and an empty directory are not
	// files the guest gets.
	writeFile(t, filepath.Join(root, "meta", ".name.tmp"), "half")
	if err := os.Symlink(filepath.Join(root, "meta", "uid"), filepath.Join(root, "meta", "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}

	files, err := readSystemInfoFiles(root)
	if err != nil {
		t.Fatalf("readSystemInfoFiles: %v", err)
	}
	got := map[string]string{}
	for _, f := range files {
		got[f.rel] = string(f.data)
		if f.perm != 0o644 {
			t.Errorf("%s perm = %v, want 0644", f.rel, f.perm)
		}
	}
	want := map[string]string{"meta/uid": "actor-1", "bundle/certs/ca.pem": "PEM"}
	if len(got) != len(want) {
		t.Fatalf("files = %v, want %v", got, want)
	}
	for rel, data := range want {
		if got[rel] != data {
			t.Errorf("%s = %q, want %q", rel, got[rel], data)
		}
	}
}

func TestPushSystemInfo(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "meta", "uid"), "actor-1")
	writeFile(t, filepath.Join(root, "bundle", "ca.pem"), "PEM")
	c := newFakeCopier()

	hashes, err := pushSystemInfo(context.Background(), c, root)
	if err != nil {
		t.Fatalf("pushSystemInfo: %v", err)
	}
	if got := c.file(guestFile("meta/uid")); got != "actor-1" {
		t.Errorf("guest meta/uid = %q, want actor-1", got)
	}
	if got := c.file(guestFile("bundle/ca.pem")); got != "PEM" {
		t.Errorf("guest bundle/ca.pem = %q, want PEM", got)
	}
	if len(hashes) != 2 {
		t.Errorf("hashes = %v, want one per file", hashes)
	}
}

func TestPushSystemInfoReturnsCopyError(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "meta", "uid"), "actor-1")
	c := newFakeCopier()
	c.err = errors.New("agent gone")
	if _, err := pushSystemInfo(context.Background(), c, root); err == nil {
		t.Fatal("pushSystemInfo = nil, want the copy error")
	}
}

// startSynced pushes root's files into a fake guest and starts syncing it.
func startSynced(t *testing.T, s *systemInfoSyncer, root string) *fakeCopier {
	t.Helper()
	c := newFakeCopier()
	hashes, err := pushSystemInfo(context.Background(), c, root)
	if err != nil {
		t.Fatalf("pushSystemInfo: %v", err)
	}
	c.copies = nil
	s.Start("actor-1", root, c, hashes)
	t.Cleanup(func() { s.Stop("actor-1") })
	return c
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestSystemInfoSyncerPushesOnlyChangedFiles(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "meta", "uid"), "actor-1")
	writeFile(t, filepath.Join(root, "bundle", "ca.pem"), "old")
	s := &systemInfoSyncer{debounce: 10 * time.Millisecond, rescan: time.Hour}
	c := startSynced(t, s, root)

	replaceFile(t, filepath.Join(root, "bundle", "ca.pem"), "new")

	waitFor(t, "the rotated bundle in the guest", func() bool { return c.file(guestFile("bundle/ca.pem")) == "new" })
	// Give a stray push of the unchanged file the chance to land.
	time.Sleep(50 * time.Millisecond)
	if got := c.copied(); !slices.Equal(got, []string{guestFile("bundle/ca.pem")}) {
		t.Errorf("copies = %v, want only the changed bundle", got)
	}
}

// The syncer starts from the hashes of the initial push, so starting it
// copies nothing that the guest already has.
func TestSystemInfoSyncerIsSeeded(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "meta", "uid"), "actor-1")
	s := &systemInfoSyncer{debounce: 10 * time.Millisecond, rescan: 20 * time.Millisecond}
	c := startSynced(t, s, root)

	time.Sleep(100 * time.Millisecond)
	if got := c.copied(); len(got) != 0 {
		t.Errorf("copies after start = %v, want none", got)
	}
}

// Start scans once, so a change made between the initial push and Start
// still reaches the guest without waiting for a rescan or an event.
func TestSystemInfoSyncerScansOnStart(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "meta", "uid"), "actor-1")
	c := newFakeCopier()
	hashes, err := pushSystemInfo(context.Background(), c, root)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "meta", "uid"), "actor-2")

	s := &systemInfoSyncer{debounce: time.Hour, rescan: time.Hour}
	s.Start("actor-1", root, c, hashes)
	defer s.Stop("actor-1")

	waitFor(t, "the change in the guest", func() bool { return c.file(guestFile("meta/uid")) == "actor-2" })
}

// Restarting the sync of a running guest, as after a failed checkpoint, knows
// nothing of what the guest has, so it pushes every file.
func TestSystemInfoSyncerStartWithoutHashesPushesEverything(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "meta", "uid"), "actor-1")
	writeFile(t, filepath.Join(root, "trust", "ca.pem"), "ca")
	c := newFakeCopier()

	s := &systemInfoSyncer{debounce: time.Hour, rescan: time.Hour}
	s.Start("actor-1", root, c, nil)
	defer s.Stop("actor-1")

	waitFor(t, "every file in the guest", func() bool {
		return c.file(guestFile("meta/uid")) == "actor-1" && c.file(guestFile("trust/ca.pem")) == "ca"
	})
}

// Stop must not wait out a push that hangs, since a checkpoint stops the sync
// before it pauses the guest. It cancels the push, and nothing is copied after
// it returns.
func TestSystemInfoSyncerStopsSynchronously(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "meta", "uid"), "actor-1")
	s := &systemInfoSyncer{debounce: time.Millisecond, rescan: 10 * time.Millisecond}
	c := newFakeCopier()
	c.block = make(chan struct{}) // never closed: every copy hangs
	c.entered = make(chan string, 1)
	s.Start("actor-1", root, c, systemInfoHashes{})

	select {
	case <-c.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the first sync never started a copy")
	}
	stopped := make(chan struct{})
	go func() {
		s.Stop("actor-1")
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop waited out a hanging copy instead of cancelling it")
	}

	writeFile(t, filepath.Join(root, "meta", "uid"), "actor-2")
	time.Sleep(100 * time.Millisecond)
	if got := c.copied(); len(got) != 0 {
		t.Errorf("copies after Stop = %v, want none", got)
	}
}

// A copy that fails keeps the guest's old hash, so a later sync copies the
// file again.
func TestSystemInfoSyncerRetriesAFailedCopy(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "bundle", "ca.pem"), "old")
	s := &systemInfoSyncer{debounce: time.Millisecond, rescan: 20 * time.Millisecond}
	c := startSynced(t, s, root)

	c.setErr(errors.New("agent unavailable"))
	before := c.attemptCount()
	replaceFile(t, filepath.Join(root, "bundle", "ca.pem"), "new")
	waitFor(t, "a failed copy of the change", func() bool { return c.attemptCount() > before })
	c.setErr(nil)

	waitFor(t, "the change in the guest after the failure", func() bool { return c.file(guestFile("bundle/ca.pem")) == "new" })
}

// A push cut short by its context must fail, so a boot or restore never starts
// the workload with some files missing.
func TestPushSystemInfoFailsWhenCutShort(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "meta", "atespace"), "team-a")
	writeFile(t, filepath.Join(root, "meta", "uid"), "actor-1")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := newFakeCopier()
	c.afterCopy = cancel

	hashes, err := pushSystemInfo(ctx, c, root)
	if !errors.Is(err, context.Canceled) || !errors.Is(err, errPushCutShort) {
		t.Errorf("pushSystemInfo cut short after %v = %v, want errPushCutShort wrapping context.Canceled", c.copied(), err)
	}
	if hashes != nil {
		t.Errorf("pushSystemInfo cut short returned hashes %v, want none", hashes)
	}
}

// Delivering a file through the agent replaces a projected file named like
// its temp file, so that file is copied again.
func TestPushSystemInfoKeepsProjectedTempFiles(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "bundle", "ca.pem"), "pem")
	writeFile(t, filepath.Join(root, "bundle", "ca.tmp"), "tmp")
	writeFile(t, filepath.Join(root, "bundle", "ca.zzz"), "zzz")
	c := newFakeCopier()
	c.agentTemps = true

	if _, err := pushSystemInfo(context.Background(), c, root); err != nil {
		t.Fatalf("pushSystemInfo: %v", err)
	}
	for rel, want := range map[string]string{"bundle/ca.pem": "pem", "bundle/ca.tmp": "tmp", "bundle/ca.zzz": "zzz"} {
		if got := c.file(guestFile(rel)); got != want {
			t.Errorf("guest %s = %q, want %q", rel, got, want)
		}
	}
}

func TestSystemInfoSyncerKeepsProjectedTempFiles(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "bundle", "ca.pem"), "old")
	writeFile(t, filepath.Join(root, "bundle", "ca.tmp"), "tmp")
	s := &systemInfoSyncer{debounce: time.Millisecond, rescan: time.Hour}
	c := newFakeCopier()
	c.agentTemps = true
	hashes, err := pushSystemInfo(context.Background(), c, root)
	if err != nil {
		t.Fatal(err)
	}
	s.Start("actor-1", root, c, hashes)
	defer s.Stop("actor-1")

	replaceFile(t, filepath.Join(root, "bundle", "ca.pem"), "new")
	waitFor(t, "the rotated file and its temp-named sibling in the guest", func() bool {
		return c.file(guestFile("bundle/ca.pem")) == "new" && c.file(guestFile("bundle/ca.tmp")) == "tmp"
	})
}

// Without fsnotify, the periodic rescan alone refreshes the guest.
func TestSystemInfoSyncerPollsWithoutFsnotify(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "meta", "uid"), "actor-1")
	s := &systemInfoSyncer{
		debounce:   time.Hour,
		rescan:     20 * time.Millisecond,
		newWatcher: func() (*fsnotify.Watcher, error) { return nil, errors.New("too many open files") },
	}
	c := startSynced(t, s, root)

	writeFile(t, filepath.Join(root, "meta", "uid"), "actor-2")

	waitFor(t, "the change in the guest", func() bool { return c.file(guestFile("meta/uid")) == "actor-2" })
}

// atelet writes through a temp file it renames over the target; the guest gets
// the target, never the temp file.
func TestSystemInfoSyncerIgnoresInFlightTempFiles(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "bundle", "ca.pem"), "old")
	s := &systemInfoSyncer{debounce: 10 * time.Millisecond, rescan: 20 * time.Millisecond}
	c := startSynced(t, s, root)

	// A write left mid-flight, then a completed one.
	writeFile(t, filepath.Join(root, "bundle", ".other.pem.tmp"), "half")
	replaceFile(t, filepath.Join(root, "bundle", "ca.pem"), "new")

	waitFor(t, "the rotated bundle in the guest", func() bool { return c.file(guestFile("bundle/ca.pem")) == "new" })
	time.Sleep(50 * time.Millisecond)
	for _, p := range c.copied() {
		if filepath.Ext(p) == ".tmp" {
			t.Errorf("copied temp file %q into the guest", p)
		}
	}
}

// A volume directory created after Start is watched too.
func TestSystemInfoSyncerWatchesNewDirectories(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "meta", "uid"), "actor-1")
	s := &systemInfoSyncer{debounce: 10 * time.Millisecond, rescan: time.Hour}
	c := startSynced(t, s, root)

	waitFor(t, "the root to be watched", func() bool {
		s.mu.Lock()
		a := s.actors["actor-1"]
		s.mu.Unlock()
		a.mu.Lock()
		defer a.mu.Unlock()
		return len(a.watched) > 0
	})
	// The new directory's creation is an event on the root; the file written
	// into it later is an event only on the new directory, and rescans are
	// off, so it arrives only if the new directory is watched.
	writeFile(t, filepath.Join(root, "later", "f"), "x")
	waitFor(t, "the new volume's file in the guest", func() bool { return c.file(guestFile("later/f")) == "x" })
	writeFile(t, filepath.Join(root, "later", "g"), "y")

	waitFor(t, "a file written into the new volume", func() bool { return c.file(guestFile("later/g")) == "y" })
}

// A CopyFile guest's containers bind their system-info volumes from the
// guest tmpfs.
func TestBuildActorContainersCopyFileSystemInfo(t *testing.T) {
	dirs := &ateworkerpb.ActorDirs{
		OciBundleDir:              t.TempDir(),
		DurableDirVolumeMountsDir: "/node/actors/a/durable-dir",
		ExternalVolumeMountsDir:   "/node/actors/a/volumes",
		SystemInfoVolumeRootsDir:  "/node/actors/a/system-info",
	}
	if err := os.MkdirAll(filepath.Join(ociBundlePath(dirs, "app"), "rootfs"), 0o755); err != nil {
		t.Fatal(err)
	}
	containers := []*ateworkerpb.Container{{
		Name:                   "app",
		ContainerSpec:          &ateworkerpb.ContainerSpec{Args: []string{"/app"}},
		SystemInfoVolumeMounts: []*ateworkerpb.VolumeMount{{Name: "sysinfo", MountPath: "/run/ate"}},
	}}
	ctrs, err := (&AteWorkerService{}).buildActorContainers("uid", dirs, containers, ocispec.SystemInfoCopyFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range ctrs[0].spec.Mounts {
		if m.Destination == "/run/ate" {
			if want := guestFile("sysinfo"); m.Source != want {
				t.Errorf("/run/ate source = %q, want %q", m.Source, want)
			}
			return
		}
	}
	t.Fatal("no /run/ate mount")
}

func TestReadSystemInfoDelivery(t *testing.T) {
	for _, tc := range []struct {
		name    string
		marker  *string
		want    ocispec.SystemInfoDelivery
		wantErr bool
	}{
		// Snapshots from builds before CopyFile delivery have no marker.
		{name: "absent", want: ocispec.SystemInfoVirtioFS},
		{name: "copyfile", marker: ptr.To("copyfile"), want: ocispec.SystemInfoCopyFile},
		{name: "virtiofs", marker: ptr.To("virtiofs"), want: ocispec.SystemInfoVirtioFS},
		{name: "garbage", marker: ptr.To("copyfile\n"), wantErr: true},
		{name: "empty", marker: ptr.To(""), wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.marker != nil {
				writeFile(t, filepath.Join(dir, systemInfoDeliveryFile), *tc.marker)
			}
			got, err := readSystemInfoDelivery(dir)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("readSystemInfoDelivery = %v, want an error", got)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Errorf("readSystemInfoDelivery = %v, %v; want %v", got, err, tc.want)
			}
		})
	}
}

// The marker comes from the activation being checkpointed, and reads back as
// the delivery it recorded.
func TestWriteSystemInfoDelivery(t *testing.T) {
	for _, d := range []ocispec.SystemInfoDelivery{ocispec.SystemInfoCopyFile, ocispec.SystemInfoVirtioFS} {
		dir := t.TempDir()
		if err := writeSystemInfoDelivery(dir, &runningActor{systemInfoDelivery: d}); err != nil {
			t.Fatalf("writeSystemInfoDelivery(%v): %v", d, err)
		}
		if got, err := readSystemInfoDelivery(dir); err != nil || got != d {
			t.Errorf("read back %v, %v; want %v", got, err, d)
		}
		// atelet ships exactly the files the checkpoint reports.
		files, err := listFiles(dir)
		if err != nil || !slices.Contains(files, systemInfoDeliveryFile) {
			t.Errorf("listFiles = %v, %v; want it to include %s", files, err, systemInfoDeliveryFile)
		}
	}
	// Without an activation, or with a delivery it can't name, the checkpoint
	// must fail rather than write a marker that may be wrong.
	if err := writeSystemInfoDelivery(t.TempDir(), nil); err == nil {
		t.Error("writeSystemInfoDelivery(nil) = nil, want an error")
	}
	if err := writeSystemInfoDelivery(t.TempDir(), &runningActor{systemInfoDelivery: 7}); err == nil {
		t.Error("writeSystemInfoDelivery(unknown) = nil, want an error")
	}
	// An activation that never set its delivery must not be recorded as
	// virtio-fs: a CopyFile guest restored that way keeps its source's files.
	if err := writeSystemInfoDelivery(t.TempDir(), &runningActor{}); err == nil {
		t.Error("writeSystemInfoDelivery(unset) = nil, want an error")
	}
}

// The system-info sync runs only for a CopyFile guest that has an agent to
// copy through and volumes to copy.
func TestSyncsSystemInfo(t *testing.T) {
	withVolumes := []*ateworkerpb.Container{{
		SystemInfoVolumeMounts: []*ateworkerpb.VolumeMount{{Name: "meta", MountPath: "/run/ate"}},
	}}
	agent := &kata.AgentClient{}
	for _, tc := range []struct {
		name       string
		ra         *runningActor
		containers []*ateworkerpb.Container
		want       bool
	}{
		{name: "no activation", containers: withVolumes},
		{name: "no agent", ra: &runningActor{systemInfoDelivery: ocispec.SystemInfoCopyFile}, containers: withVolumes},
		{name: "virtio-fs guest", ra: &runningActor{guestAgent: agent, systemInfoDelivery: ocispec.SystemInfoVirtioFS}, containers: withVolumes},
		{name: "no system-info volumes", ra: &runningActor{guestAgent: agent, systemInfoDelivery: ocispec.SystemInfoCopyFile}},
		{name: "CopyFile guest with volumes", ra: &runningActor{guestAgent: agent, systemInfoDelivery: ocispec.SystemInfoCopyFile}, containers: withVolumes, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := syncsSystemInfo(tc.ra, tc.containers); got != tc.want {
				t.Errorf("syncsSystemInfo = %v, want %v", got, tc.want)
			}
		})
	}
}

// A copy that misses the guest tmpfs must fail, not land in the host's share.
func TestStageSystemInfoCopyDirIsReadOnly(t *testing.T) {
	roottest.Require(t, "bind mounts under /run/kata-containers")
	const uid = "test-system-info-copy-ro"
	dst := filepath.Join(kata.SharedDir(uid), ocispec.ShareSystemInfoCopy)
	t.Cleanup(func() {
		kata.Unmount(dst)
		_ = os.RemoveAll(filepath.Dir(kata.SharedDir(uid)))
	})

	if err := stageSystemInfoCopyDir(uid); err != nil {
		t.Fatalf("stageSystemInfoCopyDir: %v", err)
	}
	err := os.WriteFile(filepath.Join(dst, "stray"), []byte("x"), 0o644)
	if !errors.Is(err, syscall.EROFS) {
		t.Errorf("write under the mount point = %v, want EROFS", err)
	}
	// Staging again, as a retried boot does, replaces the bind.
	if err := stageSystemInfoCopyDir(uid); err != nil {
		t.Fatalf("stageSystemInfoCopyDir again: %v", err)
	}
}

// The worker advertises the delivery every guest it boots uses, under a key
// the registration accepts from a worker.
func TestWorkerAttributes(t *testing.T) {
	got := workerAttributes()
	if v := got[hardware.AttrSystemInfoDelivery]; v != systemInfoCopyFile {
		t.Errorf("%s = %q, want %q", hardware.AttrSystemInfoDelivery, v, systemInfoCopyFile)
	}
	for k := range got {
		if !slices.Contains(hardware.WorkerAttributes, k) {
			t.Errorf("attribute %q is not a worker attribute; Report would refuse it", k)
		}
	}
}
