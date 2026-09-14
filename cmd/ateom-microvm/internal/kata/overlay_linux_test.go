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

package kata

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/agent-substrate/substrate/internal/ocispec"
)

// The container rootfs is untrusted (the image below, the guest's own snapshot
// upper above): a symlink planted at proc/sys/dev must not send the mountpoint
// mkdir somewhere else on the worker pod.
func TestEnsureOCIMountpoints(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(dir, "outside")
	rootfs := filepath.Join(dir, "rootfs")
	if err := os.MkdirAll(filepath.Join(rootfs, "realsys"), 0o755); err != nil {
		t.Fatal(err)
	}
	// proc escapes, sys is an in-rootfs symlink to an existing dir, dev is absent.
	if err := os.Symlink(filepath.Join(outside, "proc"), filepath.Join(rootfs, "proc")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("realsys", filepath.Join(rootfs, "sys")); err != nil {
		t.Fatal(err)
	}

	if err := ensureOCIMountpoints(rootfs); err != nil {
		t.Fatalf("ensureOCIMountpoints(%q) = %v", rootfs, err)
	}

	if _, err := os.Lstat(outside); !os.IsNotExist(err) {
		t.Errorf("Lstat(%q) = %v, want it never created: the symlink was followed out of the rootfs", outside, err)
	}
	if fi, err := os.Stat(filepath.Join(rootfs, "dev")); err != nil || !fi.IsDir() {
		t.Errorf("dev: Stat = %v, %v; want a directory", fi, err)
	}
	if got, err := os.Readlink(filepath.Join(rootfs, "sys")); err != nil || got != "realsys" {
		t.Errorf("sys: Readlink = %q, %v; want the in-rootfs symlink left alone", got, err)
	}
}

// The kernel requires overlay upperdir and workdir on the same filesystem and
// rejects a workdir nested inside (or equal to) upperdir — so they must be
// SIBLINGS under the container's subdirectory of the actor's upper base. The
// layout is also the snapshot tar's entry layout (<cid>/fs, <cid>/work), so a
// change here breaks every overlay mount AND every existing snapshot.
func TestUpperWorkDirsAreSiblings(t *testing.T) {
	const base = "/var/lib/ateom-gvisor/actors/uid/rootfs-upper"
	upper, work := UpperWorkDirs(base, "app")
	cidDir := filepath.Join(base, "app")
	if filepath.Dir(upper) != cidDir || filepath.Dir(work) != cidDir {
		t.Errorf("UpperWorkDirs = %q, %q; want both directly under %q", upper, work, cidDir)
	}
	if upper == work {
		t.Errorf("UpperWorkDirs: upper and work are the same directory %q", upper)
	}
	if strings.HasPrefix(work+"/", upper+"/") {
		t.Errorf("UpperWorkDirs: work %q is nested inside upper %q", work, upper)
	}
	// Tar-layout invariant: entries are <cid>/fs and <cid>/work.
	if upper != filepath.Join(base, "app", "fs") || work != filepath.Join(base, "app", "work") {
		t.Errorf("UpperWorkDirs = %q, %q; want the snapshot layout <base>/app/{fs,work}", upper, work)
	}
}

func TestVirtiofsdArgs(t *testing.T) {
	args := virtiofsdArgs(VirtiofsdOptions{
		SocketPath: "/run/vm/virtiofsd.sock",
		SharedDir:  "/run/kata-containers/shared/sandboxes/uid/shared",
	})
	if !slices.Contains(args, "--cache=auto") {
		t.Errorf("args %v do not contain --cache=auto", args)
	}
	// The host kernel owns the overlay; the guest needs no xattr passthrough, so
	// the flag must never be emitted.
	if slices.Contains(args, "--xattr") {
		t.Errorf("args %v contain --xattr; the guest has no overlay to feed it to", args)
	}
}

func TestBlockStorage(t *testing.T) {
	st := BlockStorage("/dev/vdb", "/mnt/data", "ext4")
	if st.Driver != "blk" {
		t.Errorf("Driver = %q, want blk", st.Driver)
	}
	if st.Source != "/dev/vdb" {
		t.Errorf("Source = %q, want /dev/vdb", st.Source)
	}
	if st.MountPoint != "/mnt/data" {
		t.Errorf("MountPoint = %q, want /mnt/data", st.MountPoint)
	}
	if st.Fstype != "ext4" {
		t.Errorf("Fstype = %q, want ext4", st.Fstype)
	}

	stDef := BlockStorage("/dev/vdc", "/mnt/data2", "")
	if stDef.Fstype != "ext4" {
		t.Errorf("default Fstype = %q, want ext4", stDef.Fstype)
	}
}

func TestBlockStorages(t *testing.T) {
	vols := []ocispec.BlockVolume{
		{
			HostPath:   "/dev/disk/by-id/google-vol-1",
			MountPath:  "/mnt/vol1",
			DeviceName: "/dev/vdb",
			Fstype:     "ext4",
		},
		{
			HostPath:  "/dev/disk/by-id/google-vol-2",
			MountPath: "/mnt/vol2",
		},
	}
	storages := BlockStorages(vols)
	if len(storages) != 2 {
		t.Fatalf("len(storages) = %d, want 2", len(storages))
	}
	if storages[0].Source != "/dev/vdb" || storages[0].MountPoint != "/mnt/vol1" || storages[0].Driver != "blk" || storages[0].Fstype != "ext4" {
		t.Errorf("storages[0] = %+v", storages[0])
	}
	if storages[1].Source != "/dev/vdc" || storages[1].MountPoint != "/mnt/vol2" || storages[1].Driver != "blk" || storages[1].Fstype != "ext4" {
		t.Errorf("storages[1] = %+v, want source /dev/vdc and fstype ext4", storages[1])
	}

	if empty := BlockStorages(nil); empty != nil {
		t.Errorf("BlockStorages(nil) = %v, want nil", empty)
	}
}
