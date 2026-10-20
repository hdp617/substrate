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
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/containerd/ttrpc"
	"go.ate.dev/substrate/cmd/ateworker-microvm/internal/third_party/kata/agentpb"
	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
)

// startFakeAgent serves the grpc.AgentService methods in methods and returns a
// client connected to it.
func startFakeAgent(t *testing.T, methods map[string]ttrpc.Method) *AgentClient {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "agent.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv, err := ttrpc.NewServer()
	if err != nil {
		t.Fatalf("ttrpc.NewServer: %v", err)
	}
	srv.Register("grpc.AgentService", methods)
	go func() { _ = srv.Serve(context.Background(), l) }()
	t.Cleanup(func() { _ = srv.Close() })

	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	ac := &AgentClient{conn: conn, client: ttrpc.NewClient(conn)}
	t.Cleanup(func() { _ = ac.Close() })
	return ac
}

// unaryMethod adapts handle to a ttrpc method that answers with Empty.
func unaryMethod[T any, PT interface {
	*T
	proto.Message
}](handle func(PT) error) ttrpc.Method {
	return func(_ context.Context, unmarshal func(interface{}) error) (interface{}, error) {
		req := PT(new(T))
		if err := unmarshal(req); err != nil {
			return nil, err
		}
		if err := handle(req); err != nil {
			return nil, err
		}
		return &emptypb.Empty{}, nil
	}
}

func startFakeReseed(t *testing.T, handle func(*agentpb.ReseedRandomDevRequest) error) *AgentClient {
	t.Helper()
	return startFakeAgent(t, map[string]ttrpc.Method{"ReseedRandomDev": unaryMethod(handle)})
}

// The restore path fails when this call fails, so a wrong service or method name
// here would fail every micro-VM restore.
func TestReseedRandomDevSendsData(t *testing.T) {
	got := make(chan []byte, 1)
	ac := startFakeReseed(t, func(req *agentpb.ReseedRandomDevRequest) error {
		got <- req.GetData()
		return nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	want := []byte("per-restore nonce")
	if err := ac.ReseedRandomDev(ctx, want); err != nil {
		t.Fatalf("ReseedRandomDev: %v", err)
	}
	select {
	case data := <-got:
		if !bytes.Equal(data, want) {
			t.Errorf("agent got data %q, want %q", data, want)
		}
	default:
		t.Fatal("agent never received ReseedRandomDev")
	}
}

func TestReseedRandomDevReturnsAgentError(t *testing.T) {
	ac := startFakeReseed(t, func(*agentpb.ReseedRandomDevRequest) error {
		return errors.New("reseed refused")
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := ac.ReseedRandomDev(ctx, []byte("nonce")); err == nil {
		t.Fatal("ReseedRandomDev returned nil for an agent error, want the error")
	}
}

// copyFileAgent records every CopyFile request it receives and assembles the
// files the way the agent does: chunks at their offsets, complete at FileSize.
type copyFileAgent struct {
	mu    sync.Mutex
	reqs  []*agentpb.CopyFileRequest
	files map[string][]byte
	err   error
}

func startCopyFileAgent(t *testing.T) (*AgentClient, *copyFileAgent) {
	t.Helper()
	fa := &copyFileAgent{files: map[string][]byte{}}
	ac := startFakeAgent(t, map[string]ttrpc.Method{"CopyFile": unaryMethod(func(req *agentpb.CopyFileRequest) error {
		fa.mu.Lock()
		defer fa.mu.Unlock()
		if fa.err != nil {
			return fa.err
		}
		fa.reqs = append(fa.reqs, req)
		buf := fa.files[req.GetPath()]
		if req.GetOffset() == 0 {
			buf = nil
		}
		if int64(len(buf)) != req.GetOffset() {
			return fmt.Errorf("chunk at offset %d, have %d bytes", req.GetOffset(), len(buf))
		}
		fa.files[req.GetPath()] = append(buf, req.GetData()...)
		return nil
	})})
	return ac, fa
}

const copyTestPath = guestSharedDir + "system-info-copy/meta/uid"

func TestCopyFileSingleChunk(t *testing.T) {
	ac, fa := startCopyFileAgent(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := ac.CopyFile(ctx, copyTestPath, []byte("actor-1"), 0o444); err != nil {
		t.Fatalf("CopyFile: %v", err)
	}
	if len(fa.reqs) != 1 {
		t.Fatalf("agent got %d requests, want 1", len(fa.reqs))
	}
	req := fa.reqs[0]
	if req.GetPath() != copyTestPath || req.GetFileSize() != 7 || req.GetOffset() != 0 || string(req.GetData()) != "actor-1" {
		t.Errorf("request = %v, want the whole file at offset 0", req)
	}
	// The agent treats a mode without a file type as a regular file, but the
	// kata runtime sends one, so match it.
	if req.GetFileMode() != unix.S_IFREG|0o444 {
		t.Errorf("FileMode = %o, want %o", req.GetFileMode(), unix.S_IFREG|0o444)
	}
	if req.GetDirMode()&unix.S_IFMT != unix.S_IFDIR || req.GetDirMode()&0o777 != 0o755 {
		t.Errorf("DirMode = %o, want a 0755 directory", req.GetDirMode())
	}
}

func TestCopyFileChunksLargeFile(t *testing.T) {
	ac, fa := startCopyFileAgent(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	data := bytes.Repeat([]byte("0123456789abcdef"), (2*copyFileChunkSize+100)/16)
	if err := ac.CopyFile(ctx, copyTestPath, data, 0o444); err != nil {
		t.Fatalf("CopyFile: %v", err)
	}
	if len(fa.reqs) != 3 {
		t.Fatalf("agent got %d requests, want 3", len(fa.reqs))
	}
	for i, req := range fa.reqs {
		if req.GetFileSize() != int64(len(data)) {
			t.Errorf("request %d FileSize = %d, want the whole file's %d", i, req.GetFileSize(), len(data))
		}
		if len(req.GetData()) > copyFileChunkSize {
			t.Errorf("request %d carries %d bytes, over the %d chunk size", i, len(req.GetData()), copyFileChunkSize)
		}
	}
	if !bytes.Equal(fa.files[copyTestPath], data) {
		t.Error("assembled file differs from the data sent")
	}
}

// An empty file still needs one request: the agent creates the file only when
// it sees one.
func TestCopyFileEmptyFile(t *testing.T) {
	ac, fa := startCopyFileAgent(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := ac.CopyFile(ctx, copyTestPath, nil, 0o444); err != nil {
		t.Fatalf("CopyFile: %v", err)
	}
	if len(fa.reqs) != 1 || fa.reqs[0].GetFileSize() != 0 {
		t.Fatalf("requests = %v, want one with FileSize 0", fa.reqs)
	}
}

func TestCopyFileRejectsPathOutsideShare(t *testing.T) {
	ac, fa := startCopyFileAgent(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for _, p := range []string{"/etc/passwd", "/run/kata-containers/shared/containersX/f", "relative/f"} {
		if err := ac.CopyFile(ctx, p, []byte("x"), 0o444); err == nil {
			t.Errorf("CopyFile(%q) = nil, want an error", p)
		}
	}
	if len(fa.reqs) != 0 {
		t.Errorf("agent got %d requests, want none", len(fa.reqs))
	}
}

func TestCopyFileReturnsAgentError(t *testing.T) {
	ac, fa := startCopyFileAgent(t)
	fa.err = errors.New("policy denied")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := ac.CopyFile(ctx, copyTestPath, []byte("x"), 0o444); err == nil {
		t.Fatal("CopyFile returned nil for an agent error, want the error")
	}
}

func TestCreateSandboxForActorStorages(t *testing.T) {
	virtiofs := &agentpb.Storage{Driver: virtioFSDriver, Source: FsTag, Fstype: typeVirtioFS, MountPoint: guestSharedDir}
	for _, tc := range []struct {
		name    string
		opts    CreateSandboxOpts
		want    []*agentpb.Storage
		wantErr bool
	}{
		{name: "share only", want: []*agentpb.Storage{virtiofs}},
		{
			name: "tmpfs after the share",
			opts: CreateSandboxOpts{TmpfsDir: "system-info-copy"},
			want: []*agentpb.Storage{virtiofs, {
				Driver: ephemeralDriver, Source: typeTmpfs, Fstype: typeTmpfs,
				MountPoint: guestSharedDir + "system-info-copy",
			}},
		},
		{name: "absolute tmpfs dir", opts: CreateSandboxOpts{TmpfsDir: "/tmp"}, wantErr: true},
		{name: "tmpfs dir escapes the share", opts: CreateSandboxOpts{TmpfsDir: "../x"}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []*agentpb.Storage
			ac := startFakeAgent(t, map[string]ttrpc.Method{"CreateSandbox": unaryMethod(func(req *agentpb.CreateSandboxRequest) error {
				got = req.GetStorages()
				return nil
			})})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			err := ac.CreateSandboxForActor(ctx, tc.opts)
			if tc.wantErr {
				if err == nil {
					t.Fatal("CreateSandboxForActor = nil, want an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("CreateSandboxForActor: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("storages = %v, want %v", got, tc.want)
			}
			for i := range got {
				if !proto.Equal(got[i], tc.want[i]) {
					t.Errorf("storage %d = %v, want %v", i, got[i], tc.want[i])
				}
			}
		})
	}
}
