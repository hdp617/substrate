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
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/agent-substrate/substrate/internal/ateattr"
)

func newTestInstruments(t *testing.T) (*Instruments, *sdkmetric.ManualReader) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	inst, err := NewInstruments(mp.Meter("ateom-microvm"))
	if err != nil {
		t.Fatalf("NewInstruments: %v", err)
	}
	return inst, reader
}

func collectHistogram(t *testing.T, reader *sdkmetric.ManualReader, name string) metricdata.Metrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == name {
				return m
			}
		}
	}
	t.Fatalf("metric %q not collected", name)
	return metricdata.Metrics{}
}

func phaseValues(t *testing.T, m metricdata.Metrics, phaseKey attribute.Key) map[string]float64 {
	t.Helper()
	hist, ok := m.Data.(metricdata.Histogram[float64])
	if !ok {
		t.Fatalf("%s is %T, want a float64 histogram", m.Name, m.Data)
	}
	out := make(map[string]float64, len(hist.DataPoints))
	for _, dp := range hist.DataPoints {
		v, ok := dp.Attributes.Value(phaseKey)
		if !ok {
			t.Errorf("datapoint without phase attribute: %v", dp.Attributes.ToSlice())
			continue
		}
		out[v.AsString()] = dp.Sum
	}
	return out
}

func TestCheckpointDurationShape(t *testing.T) {
	inst, reader := newTestInstruments(t)
	inst.recordCheckpoint(context.Background(),
		microVMPhase{ateattr.MicroVMCheckpointPhasePause, 10 * time.Millisecond},
		microVMPhase{ateattr.MicroVMCheckpointPhaseSnapshot, 200 * time.Millisecond},
		microVMPhase{ateattr.MicroVMCheckpointPhaseMerge, 50 * time.Millisecond},
		microVMPhase{ateattr.MicroVMCheckpointPhaseRootfsUpper, 100 * time.Millisecond},
		microVMPhase{ateattr.MicroVMCheckpointPhaseDurableDir, 0}, // skipped
		microVMPhase{ateattr.MicroVMCheckpointPhaseTeardown, 20 * time.Millisecond},
		microVMPhase{ateattr.MicroVMCheckpointPhaseTotal, 250 * time.Millisecond},
	)

	m := collectHistogram(t, reader, microVMCheckpointDurationMetric)
	if m.Unit != "s" {
		t.Errorf("unit = %q, want %q", m.Unit, "s")
	}
	byPhase := phaseValues(t, m, ateattr.MicroVMCheckpointPhaseKey)
	if _, ok := byPhase[ateattr.MicroVMCheckpointPhaseDurableDir]; ok {
		t.Error("zero-duration durable_dir phase should be absent")
	}
	if got := byPhase[ateattr.MicroVMCheckpointPhaseSnapshot]; got < 0.2 || got > 0.21 {
		t.Errorf("snapshot sum = %v, want ~0.2", got)
	}
	if len(byPhase) != 6 {
		t.Fatalf("recorded %d phases, want 6 (durable_dir skipped)", len(byPhase))
	}
}

func TestRestoreDurationShape(t *testing.T) {
	inst, reader := newTestInstruments(t)
	inst.recordRestore(context.Background(),
		microVMPhase{ateattr.MicroVMRestorePhasePrep, 5 * time.Millisecond},
		microVMPhase{ateattr.MicroVMRestorePhaseVMRestore, 80 * time.Millisecond},
		microVMPhase{ateattr.MicroVMRestorePhaseReadyz, 15 * time.Millisecond},
		microVMPhase{ateattr.MicroVMRestorePhaseTotal, 120 * time.Millisecond},
	)

	m := collectHistogram(t, reader, microVMRestoreDurationMetric)
	byPhase := phaseValues(t, m, ateattr.MicroVMRestorePhaseKey)
	if len(byPhase) != 4 {
		t.Fatalf("recorded %d phases, want 4", len(byPhase))
	}
}

func TestNilInstrumentsNoop(t *testing.T) {
	var inst *Instruments
	inst.recordCheckpoint(context.Background(), microVMPhase{ateattr.MicroVMCheckpointPhasePause, time.Second})
	inst.recordRestore(context.Background(), microVMPhase{ateattr.MicroVMRestorePhaseTotal, time.Second})
}
