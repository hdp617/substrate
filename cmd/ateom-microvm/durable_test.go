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
	"os"
	"path/filepath"
	"testing"

	"github.com/agent-substrate/substrate/internal/proto/ateompb"
)

func TestHasDurableVolumes(t *testing.T) {
	tests := []struct {
		name       string
		containers []*ateompb.Container
		want       bool
	}{
		{name: "no containers"},
		{
			name:       "container without durable volumes",
			containers: []*ateompb.Container{{Name: "app"}},
		},
		{
			name: "one of several containers has a durable volume",
			containers: []*ateompb.Container{
				{Name: "sidecar"},
				{Name: "app", DurableDirVolumeMounts: []*ateompb.DurableDirVolumeMount{
					{VolumeName: "data", MountPath: "/home/counter"},
				}},
			},
			want: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasDurableVolumes(tc.containers); got != tc.want {
				t.Errorf("hasDurableVolumes() = %v, want %v", got, tc.want)
			}
		})
	}
}

// durableDirWith returns a durable-dir volumes directory laid out the way atelet
// prepares one: a subdirectory per volume, plus optionally a stray regular file.
func durableDirWith(t *testing.T, volumes []string, strayFile bool) string {
	t.Helper()
	dir := t.TempDir()
	for _, v := range volumes {
		if err := os.Mkdir(filepath.Join(dir, v), 0o700); err != nil {
			t.Fatalf("creating volume dir %q: %v", v, err)
		}
	}
	if strayFile {
		if err := os.WriteFile(filepath.Join(dir, "not-a-volume"), nil, 0o600); err != nil {
			t.Fatalf("creating stray file: %v", err)
		}
	}
	return dir
}

func TestDurableVolumesRoundTrip(t *testing.T) {
	// Checkpoint: every volume the actor has, archived while the guest is paused.
	src := durableDirWith(t, []string{"data", "cache"}, false)
	for vol, content := range map[string]string{"data": "42", "cache": "7"} {
		if err := os.WriteFile(filepath.Join(src, vol, "a.txt"), []byte(content), 0o644); err != nil {
			t.Fatalf("writing %q content: %v", vol, err)
		}
	}
	checkpointDir := t.TempDir()
	if err := tarDurableVolumes(t.Context(), src, checkpointDir); err != nil {
		t.Fatalf("tarDurableVolumes: %v", err)
	}
	if _, err := os.Stat(filepath.Join(checkpointDir, durableTarFile)); err != nil {
		t.Fatalf("checkpoint is missing %s: %v", durableTarFile, err)
	}

	// Restore: onto the empty directory atelet re-creates for the actor.
	dst := t.TempDir()
	if err := untarDurableVolumes(dst, checkpointDir); err != nil {
		t.Fatalf("untarDurableVolumes: %v", err)
	}
	// Both volumes come back, each under its own name: the names are what the
	// guest mount paths are built from after a restore onto another node.
	for vol, want := range map[string]string{"data": "42", "cache": "7"} {
		got, err := os.ReadFile(filepath.Join(dst, vol, "a.txt"))
		if err != nil {
			t.Errorf("reading restored %q content: %v", vol, err)
			continue
		}
		if string(got) != want {
			t.Errorf("restored %q content = %q, want %q", vol, got, want)
		}
	}
}

func TestDurableRawDisksRoundTrip(t *testing.T) {
	t.Setenv(durableBackendEnvVar, durableBackendRawDisk)

	src := t.TempDir()
	checkpointDir := t.TempDir()
	dst := t.TempDir()

	// Create raw disk images in src directory.
	volFiles := map[string]string{
		"workspace.raw": "ext4-dummy-header-1234",
		"cache.raw":     "ext4-dummy-header-5678",
	}
	for name, content := range volFiles {
		p := filepath.Join(src, name)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatalf("writing %q: %v", name, err)
		}
	}

	// Capture: under raw-disk, files are hard-linked in sub-millisecond time.
	if err := captureDurableVolumes(t.Context(), src, checkpointDir); err != nil {
		t.Fatalf("captureDurableVolumes: %v", err)
	}

	for name := range volFiles {
		srcPath := filepath.Join(src, name)
		snapPath := filepath.Join(checkpointDir, name)

		srcStat, err := os.Stat(srcPath)
		if err != nil {
			t.Fatalf("stat src %q: %v", srcPath, err)
		}
		snapStat, err := os.Stat(snapPath)
		if err != nil {
			t.Fatalf("stat snapshot %q: %v", snapPath, err)
		}
		if !os.SameFile(srcStat, snapStat) {
			t.Errorf("%s was copied rather than hardlinked into the checkpoint", name)
		}
	}

	// Restore: hardlinks or copies from snapshot into destination.
	if err := restoreDurableVolumes(dst, checkpointDir); err != nil {
		t.Fatalf("restoreDurableVolumes: %v", err)
	}

	for name, want := range volFiles {
		got, err := os.ReadFile(filepath.Join(dst, name))
		if err != nil {
			t.Errorf("reading restored %q: %v", name, err)
			continue
		}
		if string(got) != want {
			t.Errorf("restored %q = %q, want %q", name, got, want)
		}
	}
}

func TestStageDurableRawDisks(t *testing.T) {
	t.Setenv(durableBackendEnvVar, durableBackendRawDisk)

	s := &AteomService{}
	actorUID := "test-actor-raw-dur"
	containers := []*ateompb.Container{
		{
			Name: "agent",
			DurableDirVolumeMounts: []*ateompb.DurableDirVolumeMount{
				{VolumeName: "workspace", MountPath: "/workspace"},
			},
		},
	}

	tmpDir := t.TempDir()
	vols, err := s.stageDurableVolumesAt(t.Context(), tmpDir, actorUID, containers)
	if err != nil {
		t.Fatalf("stageDurableVolumesAt: %v", err)
	}

	if len(vols) != 1 {
		t.Fatalf("expected 1 BlockVolume, got %d", len(vols))
	}
	bv := vols[0]
	if bv.Name != "workspace" {
		t.Errorf("Name = %q, want %q", bv.Name, "workspace")
	}
	if bv.MountPath != "/workspace" {
		t.Errorf("MountPath = %q, want %q", bv.MountPath, "/workspace")
	}
	if bv.DeviceName != "/dev/vdb" {
		t.Errorf("DeviceName = %q, want %q", bv.DeviceName, "/dev/vdb")
	}
	if bv.Fstype != "ext4" {
		t.Errorf("Fstype = %q, want %q", bv.Fstype, "ext4")
	}
	if !slicesContains(bv.Options, "discard") {
		t.Errorf("Options %v does not contain discard", bv.Options)
	}

	// Verify disk file was created on host.
	if st, err := os.Stat(bv.HostPath); err != nil || st.Size() == 0 {
		t.Errorf("expected non-empty virtual disk at %q, err: %v", bv.HostPath, err)
	}
}

func slicesContains(ss []string, target string) bool {
	for _, s := range ss {
		if s == target {
			return true
		}
	}
	return false
}
