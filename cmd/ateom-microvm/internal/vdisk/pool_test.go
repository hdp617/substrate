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

package vdisk

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestPoolAllocate_SparseAndFormat(t *testing.T) {
	poolDir := t.TempDir()
	p, err := NewPool(poolDir)
	if err != nil {
		t.Fatalf("NewPool() = %v", err)
	}

	const (
		actorUID   = "actor-abc-123"
		volumeName = "data"
		mountPath  = "/data"
		sizeBytes  = 64 * 1024 * 1024 // 64 MiB for quick test
	)

	ctx := context.Background()
	disk, err := p.Allocate(ctx, AllocateOptions{
		ActorUID:   actorUID,
		VolumeName: volumeName,
		SizeBytes:  sizeBytes,
		MountPath:  mountPath,
		Fstype:     "ext4",
		Readonly:   false,
	})
	if err != nil {
		t.Fatalf("Allocate() = %v", err)
	}

	if disk.HostPath != p.DiskPath(actorUID, volumeName) {
		t.Errorf("HostPath = %q, want %q", disk.HostPath, p.DiskPath(actorUID, volumeName))
	}
	if disk.MountPath != mountPath {
		t.Errorf("MountPath = %q, want %q", disk.MountPath, mountPath)
	}
	if !containsOption(disk.Options, "discard") {
		t.Errorf("Options = %v, want discard option included", disk.Options)
	}

	allocated, logical, err := DiskUsage(disk.HostPath)
	if err != nil {
		t.Fatalf("DiskUsage() = %v", err)
	}
	if logical != sizeBytes {
		t.Errorf("logical size = %d, want %d", logical, sizeBytes)
	}
	t.Logf("Virtual disk allocated: %d physical bytes, %d logical bytes", allocated, logical)

	// Test ToBlockVolume conversion
	bv := disk.ToBlockVolume(0)
	if bv.Name != volumeName {
		t.Errorf("bv.Name = %q, want %q", bv.Name, volumeName)
	}
	if bv.DeviceName != "/dev/vdb" {
		t.Errorf("bv.DeviceName = %q, want /dev/vdb", bv.DeviceName)
	}
	if bv.HostPath != disk.HostPath {
		t.Errorf("bv.HostPath = %q, want %q", bv.HostPath, disk.HostPath)
	}
	if !containsOption(bv.Options, "discard") {
		t.Errorf("bv.Options = %v, want discard", bv.Options)
	}

	// Idempotency check: second Allocate should reuse existing disk without error
	disk2, err := p.Allocate(ctx, AllocateOptions{
		ActorUID:   actorUID,
		VolumeName: volumeName,
		MountPath:  mountPath,
	})
	if err != nil {
		t.Fatalf("second Allocate() = %v", err)
	}
	if disk2.HostPath != disk.HostPath {
		t.Errorf("reallocated disk.HostPath = %q, want %q", disk2.HostPath, disk.HostPath)
	}
}

func TestPoolAllocate_TemplateCloning(t *testing.T) {
	poolDir := t.TempDir()
	templateDir := t.TempDir()
	templatePath := filepath.Join(templateDir, "empty-template.raw")

	payload := bytes.Repeat([]byte("EXT4_GOLDEN_MAGIC_DATA_12345"), 1024)
	if err := os.WriteFile(templatePath, payload, 0o644); err != nil {
		t.Fatalf("writing template file: %v", err)
	}

	p, err := NewPool(poolDir, WithTemplate(templatePath))
	if err != nil {
		t.Fatalf("NewPool() = %v", err)
	}

	ctx := context.Background()
	disk, err := p.Allocate(ctx, AllocateOptions{
		ActorUID:   "actor-cloned-1",
		VolumeName: "appdata",
		MountPath:  "/app",
		SizeBytes:  int64(len(payload)),
	})
	if err != nil {
		t.Fatalf("Allocate() from template = %v", err)
	}

	clonedData, err := os.ReadFile(disk.HostPath)
	if err != nil {
		t.Fatalf("reading cloned disk: %v", err)
	}
	if !bytes.Equal(clonedData, payload) {
		t.Errorf("cloned data does not match template")
	}
}

func TestPoolPunchHole_ReclaimsSpace(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "sparse-punch.raw")

	// Create 10 MiB sparse file
	const fileSize = 10 * 1024 * 1024
	f, err := os.Create(filePath)
	if err != nil {
		t.Fatalf("Create() = %v", err)
	}
	if err := f.Truncate(fileSize); err != nil {
		f.Close()
		t.Fatalf("Truncate() = %v", err)
	}

	// Write 1 MiB of non-zero data in the middle (offset 2 MiB)
	payload := bytes.Repeat([]byte("ABCD1234"), 128*1024) // 1 MiB
	if _, err := f.WriteAt(payload, 2*1024*1024); err != nil {
		f.Close()
		t.Fatalf("WriteAt() = %v", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		t.Fatalf("Sync() = %v", err)
	}
	f.Close()

	allocBefore, _, err := DiskUsage(filePath)
	if err != nil {
		t.Fatalf("DiskUsage before = %v", err)
	}
	if allocBefore == 0 {
		t.Skip("Underlying filesystem did not allocate physical blocks; skipping punch hole test")
	}

	// Punch hole through the written 1 MiB range
	if err := PunchHole(filePath, 2*1024*1024, int64(len(payload))); err != nil {
		t.Fatalf("PunchHole() = %v", err)
	}

	allocAfter, _, err := DiskUsage(filePath)
	if err != nil {
		t.Fatalf("DiskUsage after = %v", err)
	}

	t.Logf("Allocated before punch: %d, after punch: %d", allocBefore, allocAfter)
	if allocAfter >= allocBefore {
		t.Errorf("PunchHole failed to reduce physical allocation: before=%d, after=%d", allocBefore, allocAfter)
	}
}

func TestPoolRelease(t *testing.T) {
	poolDir := t.TempDir()
	p, err := NewPool(poolDir)
	if err != nil {
		t.Fatalf("NewPool() = %v", err)
	}

	ctx := context.Background()
	disk, err := p.Allocate(ctx, AllocateOptions{
		ActorUID:   "actor-del",
		VolumeName: "scratch",
		MountPath:  "/tmp/scratch",
		SizeBytes:  1024 * 1024,
	})
	if err != nil {
		t.Fatalf("Allocate() = %v", err)
	}

	if _, err := os.Stat(disk.HostPath); err != nil {
		t.Fatalf("disk file does not exist: %v", err)
	}

	if err := p.Release(ctx, "actor-del", "scratch"); err != nil {
		t.Fatalf("Release() = %v", err)
	}

	if _, err := os.Stat(disk.HostPath); !os.IsNotExist(err) {
		t.Errorf("disk file still exists after Release(): %v", err)
	}
}
