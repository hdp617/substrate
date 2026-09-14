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

// Durable-dir volumes for the micro-VM runtime.
//
// A durable-dir volume is a directory whose contents outlive the actor's process
// state: it survives suspend/resume and, under the Data snapshot scope, is the
// ONLY thing captured (the workload cold-starts on restore). The host side is
// owned by atelet, which creates one directory per volume under
// ateompath.DurableDirVolumeMountsDir(actorUID) and wipes them when the actor's
// directories are reset.
//
// There are two storage arrangements supported, chosen by ATEOM_DURABLE_BACKEND:
//
//   - tar (default): Exposed to the guest via virtio-fs under SharedDir/durable.
//     Snapshots carry the contents as a tarball of the volume directory.
//   - raw-disk: Provisioned as raw sparse ext4 disk images on the host and
//     attached directly to the guest as virtio-blk devices. Snapshots hardlink
//     the disk image file in sub-millisecond time.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/agent-substrate/substrate/cmd/ateom-microvm/internal/kata"
	"github.com/agent-substrate/substrate/internal/ateompath"
	"github.com/agent-substrate/substrate/internal/ocispec"
	"github.com/agent-substrate/substrate/internal/proto/ateompb"
	"github.com/agent-substrate/substrate/internal/tarutil"
	"golang.org/x/sys/unix"
)

const (
	durableTarFile          = ateompath.DurableDirTarFile
	durableRawSuffix        = ateompath.DurableDirRawSuffix
	durableBackendEnvVar    = "ATEOM_DURABLE_BACKEND"
	durableBackendRawDisk   = "raw-disk"
	durableBackendTar       = "tar"
	defaultRawDiskSizeBytes = 10 * 1024 * 1024 * 1024 // 10 GiB
)

// isRawDiskDurableBackend reports whether durable-dir volumes should be backed
// by raw sparse virtual disks over virtio-blk instead of virtio-fs.
func isRawDiskDurableBackend() bool {
	return os.Getenv(durableBackendEnvVar) == durableBackendRawDisk
}

// hasDurableVolumes reports whether any container mounts a durable-dir volume.
func hasDurableVolumes(containers []*ateompb.Container) bool {
	for _, c := range containers {
		if len(c.GetDurableDirVolumeMounts()) > 0 {
			return true
		}
	}
	return false
}

// stageDurableVolumes prepares the actor's durable-dir volumes for execution.
// Under virtio-fs, it bind-mounts the host directory into the shared tree.
// Under raw-disk, it provisions sparse ext4 disk files and returns them as
// direct-attach BlockVolumes.
func (s *AteomService) stageDurableVolumes(ctx context.Context, actorUID string, containers []*ateompb.Container) ([]ocispec.BlockVolume, error) {
	return s.stageDurableVolumesAt(ctx, ateompath.DurableDirVolumeMountsDir(actorUID), actorUID, containers)
}

func (s *AteomService) stageDurableVolumesAt(ctx context.Context, dir, actorUID string, containers []*ateompb.Container) ([]ocispec.BlockVolume, error) {
	if isRawDiskDurableBackend() {
		return s.stageDurableRawDisksAt(ctx, dir, containers)
	}

	if _, err := os.Stat(dir); err != nil {
		return nil, fmt.Errorf("while checking durable-dir volumes dir %q: %w", dir, err)
	}
	if err := kata.BindIntoShare(ctx, dir, actorUID, ocispec.ShareDurable); err != nil {
		return nil, fmt.Errorf("while binding durable-dir volumes into the shared tree: %w", err)
	}
	return nil, nil
}

// stageDurableRawDisksAt allocates raw sparse ext4 images for declared durable volumes under dir.
func (s *AteomService) stageDurableRawDisksAt(ctx context.Context, dir string, containers []*ateompb.Container) ([]ocispec.BlockVolume, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("creating durable-dir volumes dir %q: %w", dir, err)
	}

	var vols []ocispec.BlockVolume
	seen := map[string]bool{}
	for _, c := range containers {
		for _, m := range c.GetDurableDirVolumeMounts() {
			volName := m.GetVolumeName()
			if volName == "" {
				volName = "durable-dir"
			}
			diskPath := filepath.Join(dir, volName+durableRawSuffix)
			if !seen[volName] {
				seen[volName] = true
				if _, err := os.Stat(diskPath); errors.Is(err, os.ErrNotExist) {
					if err := createAndFormatDisk(ctx, diskPath, defaultRawDiskSizeBytes, "ext4"); err != nil {
						return nil, fmt.Errorf("creating raw virtual disk for %q: %w", volName, err)
					}
				}
			}

			devIdx := len(s.blockVolumes) + len(vols)
			bv := ocispec.BlockVolume{
				Name:       volName,
				HostPath:   diskPath,
				MountPath:  m.GetMountPath(),
				DeviceName: fmt.Sprintf("/dev/vd%c", 'b'+rune(devIdx)),
				Fstype:     "ext4",
				Readonly:   false,
				Options:    []string{"discard", "noatime"},
			}
			vols = append(vols, bv)
		}
	}
	return vols, nil
}

// captureDurableVolumes writes the actor's durable-dir volumes into the checkpoint directory.
func captureDurableVolumes(ctx context.Context, dir, checkpointDir string) error {
	if isRawDiskDurableBackend() {
		return captureDurableRawDisks(dir, checkpointDir)
	}
	return tarDurableVolumes(ctx, dir, checkpointDir)
}

func captureDurableRawDisks(dir, checkpointDir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("reading durable directory %q: %w", dir, err)
	}

	found := false
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), durableRawSuffix) {
			src := filepath.Join(dir, entry.Name())
			dst := filepath.Join(checkpointDir, entry.Name())
			if err := linkOrCopyRawDisk(src, dst); err != nil {
				return fmt.Errorf("capturing durable raw disk %q: %w", entry.Name(), err)
			}
			found = true
		}
	}
	if !found {
		return tarDurableVolumes(context.Background(), dir, checkpointDir)
	}
	return nil
}

// restoreDurableVolumes restores the durable-dir volumes from a snapshot into the actor's host directory.
func restoreDurableVolumes(dir, snapshotDir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("while creating durable-dir volumes dir %q: %w", dir, err)
	}
	entries, err := os.ReadDir(snapshotDir)
	if err != nil {
		return fmt.Errorf("while reading snapshot dir %q: %w", snapshotDir, err)
	}

	rawFound := false
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), durableRawSuffix) {
			src := filepath.Join(snapshotDir, entry.Name())
			dst := filepath.Join(dir, entry.Name())
			if err := linkOrCopyRawDisk(src, dst); err != nil {
				return fmt.Errorf("restoring durable raw disk %q: %w", entry.Name(), err)
			}
			rawFound = true
		}
	}
	if rawFound {
		return nil
	}

	if _, err := os.Stat(filepath.Join(snapshotDir, durableTarFile)); err == nil {
		return untarDurableVolumes(dir, snapshotDir)
	}
	return nil
}

func tarDurableVolumes(ctx context.Context, dir, checkpointDir string) error {
	if err := tarutil.Create(ctx, filepath.Join(checkpointDir, durableTarFile), dir); err != nil {
		return fmt.Errorf("while archiving durable-dir volumes from %q: %w", dir, err)
	}
	return nil
}

func untarDurableVolumes(dir, snapshotDir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("while creating durable-dir volumes dir %q: %w", dir, err)
	}
	if err := tarutil.Extract(filepath.Join(snapshotDir, durableTarFile), dir); err != nil {
		return fmt.Errorf("while restoring durable-dir volumes into %q: %w", dir, err)
	}
	return nil
}

func linkOrCopyRawDisk(src, dst string) error {
	err := os.Link(src, dst)
	if errors.Is(err, syscall.EXDEV) || errors.Is(err, syscall.EMLINK) || errors.Is(err, syscall.EPERM) {
		return copySparseFile(src, dst)
	}
	if err != nil {
		return err
	}
	return nil
}

func copySparseFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()

	stat, err := in.Stat()
	if err != nil {
		return err
	}
	if err := out.Truncate(stat.Size()); err != nil {
		return err
	}

	buf := make([]byte, 2*1024*1024)
	var offset int64
	for offset < stat.Size() {
		dataOffset, err := unix.Seek(int(in.Fd()), offset, unix.SEEK_DATA)
		if err != nil {
			if errors.Is(err, unix.ENXIO) {
				break
			}
			return err
		}
		holeOffset, err := unix.Seek(int(in.Fd()), dataOffset, unix.SEEK_HOLE)
		if err != nil {
			holeOffset = stat.Size()
		}

		length := holeOffset - dataOffset
		if _, err := in.Seek(dataOffset, io.SeekStart); err != nil {
			return err
		}
		if _, err := out.Seek(dataOffset, io.SeekStart); err != nil {
			return err
		}

		for length > 0 {
			toRead := min(int64(len(buf)), length)
			n, err := in.Read(buf[:toRead])
			if n > 0 {
				if _, wErr := out.Write(buf[:n]); wErr != nil {
					return wErr
				}
				length -= int64(n)
			}
			if err != nil {
				if errors.Is(err, io.EOF) {
					break
				}
				return err
			}
		}
		offset = holeOffset
	}
	return nil
}

func createAndFormatDisk(ctx context.Context, path string, sizeBytes int64, fstype string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o644)
	if err != nil {
		return fmt.Errorf("creating file %q: %w", path, err)
	}
	defer f.Close()

	if err := unix.Fallocate(int(f.Fd()), 0, 0, sizeBytes); err != nil {
		if err := f.Truncate(sizeBytes); err != nil {
			return fmt.Errorf("truncating file to %d bytes: %w", sizeBytes, err)
		}
	}

	if fstype == "ext4" {
		cmd := exec.CommandContext(ctx, "/usr/sbin/mkfs.ext4", "-F", "-q", "-m", "0", "-b", "4096",
			"-E", "lazy_itable_init=1,lazy_journal_init=1", "-O", "^has_journal", path)
		var stderr strings.Builder
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("mkfs.ext4 on %q: %w (%s)", path, err, strings.TrimSpace(stderr.String()))
		}
	}
	return nil
}
