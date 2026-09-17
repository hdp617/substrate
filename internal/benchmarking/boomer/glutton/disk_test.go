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

package glutton

import (
	"context"
	"reflect"
	"testing"

	"github.com/agent-substrate/substrate/internal/benchmarking/boomer/dynconfig"
	"github.com/agent-substrate/substrate/internal/benchmarking/boomer/userclass"
	"github.com/agent-substrate/substrate/internal/benchmarking/glutton/fake"
)

func TestDiskUserDefaultsToGluttonTemplate(t *testing.T) {
	srv := &fake.Server{Data: []byte("rootfs")}
	fakeCtrl := &fakeControlClient{}
	cfg := newTestConfig(t, srv, &userclass.Config{
		APIStub: fakeCtrl,
		Dyn: dynconfig.NewHolder(dynconfig.Config{
			DurDirFileSize: int64(len(srv.Data)),
			ResumeMode:     dynconfig.ResumeModeExplicit,
		}),
	})

	rt := &diskRuntime{cfg: cfg}
	u, err := rt.startUser(context.Background(), cfg.Dyn.Load())
	if err != nil {
		t.Fatalf("startUser failed: %v", err)
	}
	if u.templateName != defaultDiskTemplate {
		t.Errorf("templateName = %q, want %q", u.templateName, defaultDiskTemplate)
	}
	if u.userClass != diskUserClass {
		t.Errorf("userClass = %q, want %q", u.userClass, diskUserClass)
	}
	if u.metricAfterResume != "ReadDiskAfterResume" {
		t.Errorf("metricAfterResume = %q, want ReadDiskAfterResume", u.metricAfterResume)
	}
}

func TestDiskLoopSequence(t *testing.T) {
	srv := &fake.Server{Data: []byte("seq content")}
	fakeCtrl := &fakeControlClient{}
	cfg := &userclass.Config{
		APIStub: fakeCtrl,
		Dyn: dynconfig.NewHolder(dynconfig.Config{
			ResumeMode: dynconfig.ResumeModeExplicit,
		}),
	}
	c := newTestConfig(t, srv, cfg)
	u := &durDirUser{
		cfg:          c,
		actorName:    "diskactor",
		templateName: defaultDiskTemplate,
		userClass:    diskUserClass,
		expectedSize: int64(len(srv.Data)),
	}
	u.metricWrite, u.metricServeInitial, u.metricAfterResume, u.metricWarm, u.metricOverwrite = defaultDiskMetrics()
	u.expectedDigest = srv.HexDigest()

	u.step(context.Background(), c.Dyn.Load())

	wantGRPC := []string{"SuspendActor", "ResumeActor"}
	if got := fakeCtrl.recordedCalls(); !reflect.DeepEqual(got, wantGRPC) {
		t.Errorf("gRPC calls: got %v, want %v", got, wantGRPC)
	}
	wantHTTP := []string{fake.ReadDiskRoute, fake.ReadDiskRoute, fake.WriteDiskRoute}
	if got := srv.RecordedPaths(); !reflect.DeepEqual(got, wantHTTP) {
		t.Errorf("HTTP calls: got %v, want %v", got, wantHTTP)
	}
}
