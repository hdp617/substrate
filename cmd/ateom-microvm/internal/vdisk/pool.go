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

// Package vdisk provides virtual disk management for micro-virtual machines,
// allocating sparse disk image files on a host filesystem pool and exposing them
// via virtio-blk devices.
package vdisk

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/agent-substrate/substrate/internal/ocispec"
	"golang.org/x/sys/unix"
)

const (
	// DefaultDiskSizeBytes is the fallback virtual disk size (10 GiB).
	DefaultDiskSizeBytes = 10 * 1024 * 1024 * 1024

	// Linux fallocate flags for hole-punching space reclamation.
	fallocFlKeepSize  = 0x01
	fallocFlPunchHole = 0x02
)

// Pool manages virtual disk files allocated on a host backing filesystem.
type Pool struct {
	dir          string
	templatePath string
}

// PoolOption configures a Pool instance.
type PoolOption func(*Pool)

// WithTemplate configures a pre-formatted empty ext4 golden template image for
// sub-millisecond cloning via ioctl(FICLONE).
func WithTemplate(path string) PoolOption {
	return func(p *Pool) {
		p.templatePath = path
	}
}

// NewPool creates a new virtual disk pool rooted at dir.
func NewPool(dir string, opts ...PoolOption) (*Pool, error) {
	if dir == "" {
		return nil, errors.New("virtual disk pool directory cannot be empty")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("creating virtual disk pool dir %q: %w", dir, err)
	}
	p := &Pool{dir: dir}
	for _, opt := range opts {
		opt(p)
	}
	return p, nil
}

// AllocateOptions specifies the parameters for allocating a virtual disk.
type AllocateOptions struct {
	ActorUID     string
	VolumeName   string
	SizeBytes    int64
	MountPath    string
	Fstype       string
	Readonly     bool
	Options      []string
	TemplatePath string // Optional per-volume template override
}

// VirtualDisk represents an allocated disk image on the host filesystem.
type VirtualDisk struct {
	ActorUID   string
	VolumeName string
	HostPath   string
	MountPath  string
	SizeBytes  int64
	Fstype     string
	Readonly   bool
	Options    []string
}

// ToBlockVolume converts a VirtualDisk into an ocispec.BlockVolume for injection
// into Cloud Hypervisor and kata-agent.
func (v *VirtualDisk) ToBlockVolume(deviceIndex int) ocispec.BlockVolume {
	return ocispec.BlockVolume{
		Name:       v.VolumeName,
		HostPath:   v.HostPath,
		MountPath:  v.MountPath,
		DeviceName: fmt.Sprintf("/dev/vd%c", 'b'+rune(deviceIndex)),
		Fstype:     v.Fstype,
		Readonly:   v.Readonly,
		Options:    v.Options,
	}
}

// DiskPath returns the expected on-disk path for an actor's virtual disk.
func (p *Pool) DiskPath(actorUID, volumeName string) string {
	return filepath.Join(p.dir, actorUID, volumeName+".raw")
}

// Allocate ensures a virtual disk image exists for the specified actor and volume.
// If the image already exists, it is reused. If the host filesystem supports
// reflinks (e.g. XFS) and a template is configured, it clones the template via
// ioctl(FICLONE) in sub-millisecond time.
func (p *Pool) Allocate(ctx context.Context, opts AllocateOptions) (*VirtualDisk, error) {
	if opts.ActorUID == "" {
		return nil, errors.New("ActorUID cannot be empty")
	}
	if opts.VolumeName == "" {
		return nil, errors.New("VolumeName cannot be empty")
	}
	if opts.MountPath == "" {
		return nil, errors.New("MountPath cannot be empty")
	}
	if opts.SizeBytes <= 0 {
		opts.SizeBytes = DefaultDiskSizeBytes
	}
	if opts.Fstype == "" {
		opts.Fstype = "ext4"
	}

	hostPath := p.DiskPath(opts.ActorUID, opts.VolumeName)
	actorDir := filepath.Dir(hostPath)

	// Ensure actor directory exists.
	if err := os.MkdirAll(actorDir, 0o755); err != nil {
		return nil, fmt.Errorf("creating actor disk directory %q: %w", actorDir, err)
	}

	// If disk image already exists, return existing descriptor.
	if info, err := os.Stat(hostPath); err == nil && !info.IsDir() {
		mountOpts := append([]string(nil), opts.Options...)
		if !opts.Readonly && !containsOption(mountOpts, "discard") {
			mountOpts = append(mountOpts, "discard")
		}
		return &VirtualDisk{
			ActorUID:   opts.ActorUID,
			VolumeName: opts.VolumeName,
			HostPath:   hostPath,
			MountPath:  opts.MountPath,
			SizeBytes:  info.Size(),
			Fstype:     opts.Fstype,
			Readonly:   opts.Readonly,
			Options:    mountOpts,
		}, nil
	}

	template := opts.TemplatePath
	if template == "" {
		template = p.templatePath
	}

	cloned := false
	if template != "" {
		if err := cloneTemplate(template, hostPath); err == nil {
			cloned = true
		}
	}

	if !cloned {
		if err := createAndFormatDisk(ctx, hostPath, opts.SizeBytes, opts.Fstype); err != nil {
			_ = os.Remove(hostPath)
			return nil, fmt.Errorf("creating disk image %q: %w", hostPath, err)
		}
	}

	mountOpts := append([]string(nil), opts.Options...)
	if !opts.Readonly && !containsOption(mountOpts, "discard") {
		mountOpts = append(mountOpts, "discard")
	}

	return &VirtualDisk{
		ActorUID:   opts.ActorUID,
		VolumeName: opts.VolumeName,
		HostPath:   hostPath,
		MountPath:  opts.MountPath,
		SizeBytes:  opts.SizeBytes,
		Fstype:     opts.Fstype,
		Readonly:   opts.Readonly,
		Options:    mountOpts,
	}, nil
}

// Release removes a virtual disk and cleans up its actor directory if empty.
func (p *Pool) Release(ctx context.Context, actorUID, volumeName string) error {
	hostPath := p.DiskPath(actorUID, volumeName)
	if err := os.Remove(hostPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing virtual disk %q: %w", hostPath, err)
	}
	// Attempt to remove actor directory; ignore error if not empty.
	_ = os.Remove(filepath.Dir(hostPath))
	return nil
}

// PunchHole deallocates physical blocks in the specified file range using
// fallocate(FALLOC_FL_PUNCH_HOLE | FALLOC_FL_KEEP_SIZE).
func PunchHole(path string, offset, length int64) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		return fmt.Errorf("opening file %q for hole punching: %w", path, err)
	}
	defer f.Close()

	if err := unix.Fallocate(int(f.Fd()), fallocFlPunchHole|fallocFlKeepSize, offset, length); err != nil {
		return fmt.Errorf("fallocate punch hole on %q [offset=%d, len=%d]: %w", path, offset, length, err)
	}
	return nil
}

// DiskUsage returns the physical allocated bytes (blocks * 512) and logical file size.
func DiskUsage(path string) (allocatedBytes, logicalBytes int64, err error) {
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		return 0, 0, fmt.Errorf("stat %q: %w", path, err)
	}
	return st.Blocks * 512, st.Size, nil
}

// cloneTemplate attempts an instant reflink copy via ioctl(FICLONE),
// falling back to a file copy if reflinks are unsupported on the filesystem.
func cloneTemplate(srcPath, dstPath string) error {
	src, err := os.Open(srcPath)
	if err != nil {
		return fmt.Errorf("opening template %q: %w", srcPath, err)
	}
	defer src.Close()

	dst, err := os.OpenFile(dstPath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o644)
	if err != nil {
		return fmt.Errorf("creating destination %q: %w", dstPath, err)
	}
	defer dst.Close()

	if err := unix.IoctlFileClone(int(dst.Fd()), int(src.Fd())); err == nil {
		return nil
	}

	// Fallback to copy when the backing filesystem does not support reflinks (e.g. ext4 or tmpfs).
	if _, err := io.Copy(dst, src); err != nil {
		_ = os.Remove(dstPath)
		return fmt.Errorf("copying template %q -> %q: %w", srcPath, dstPath, err)
	}
	return nil
}

// createAndFormatDisk creates a sparse image file and formats it with ext4 or xfs.
func createAndFormatDisk(ctx context.Context, path string, sizeBytes int64, fstype string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o644)
	if err != nil {
		return fmt.Errorf("creating file %q: %w", path, err)
	}
	defer f.Close()

	// Try fallocate to establish logical bounds; fall back to Truncate if fallocate fails.
	if err := unix.Fallocate(int(f.Fd()), 0, 0, sizeBytes); err != nil {
		if err := f.Truncate(sizeBytes); err != nil {
			return fmt.Errorf("truncating file to %d bytes: %w", sizeBytes, err)
		}
	}

	// Format filesystem if ext4.
	if fstype == "ext4" {
		cmd := exec.CommandContext(ctx, "mkfs.ext4", "-F", "-q", path)
		var stderr strings.Builder
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("mkfs.ext4 on %q: %w (%s)", path, err, strings.TrimSpace(stderr.String()))
		}
	}
	return nil
}

func containsOption(opts []string, target string) bool {
	for _, o := range opts {
		if o == target {
			return true
		}
	}
	return false
}
