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
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/agent-substrate/substrate/internal/ateattr"
)

const (
	microVMCheckpointDurationMetric = "ate.microvm.checkpoint.duration"
	microVMRestoreDurationMetric    = "ate.microvm.restore.duration"
)

// Same bucket set as atelet's snapshot-phase histograms: warm paths land in
// single-digit milliseconds; a multi-GiB rootfs-upper tar or OnDemand merge
// can run for tens of seconds.
var microVMPhaseBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 20, 30, 60}

// Instruments holds ateom-microvm's suspend/resume phase histograms. A nil
// *Instruments is a valid no-op, so call sites need no guard.
type Instruments struct {
	checkpointDuration metric.Float64Histogram
	restoreDuration    metric.Float64Histogram
}

func NewInstruments(meter metric.Meter) (*Instruments, error) {
	checkpointDuration, err := meter.Float64Histogram(
		microVMCheckpointDurationMetric,
		metric.WithUnit("s"),
		metric.WithDescription("Duration of one phase of a micro-VM actor checkpoint inside ateom-microvm. Phases that did not run are absent."),
		metric.WithExplicitBucketBoundaries(microVMPhaseBuckets...),
	)
	if err != nil {
		return nil, fmt.Errorf("create %s histogram: %w", microVMCheckpointDurationMetric, err)
	}
	restoreDuration, err := meter.Float64Histogram(
		microVMRestoreDurationMetric,
		metric.WithUnit("s"),
		metric.WithDescription("Duration of one phase of a micro-VM actor restore inside ateom-microvm. Phases that did not run are absent."),
		metric.WithExplicitBucketBoundaries(microVMPhaseBuckets...),
	)
	if err != nil {
		return nil, fmt.Errorf("create %s histogram: %w", microVMRestoreDurationMetric, err)
	}
	return &Instruments{
		checkpointDuration: checkpointDuration,
		restoreDuration:    restoreDuration,
	}, nil
}

type microVMPhase struct {
	name string
	d    time.Duration
}

func (i *Instruments) recordCheckpoint(ctx context.Context, phases ...microVMPhase) {
	if i == nil || i.checkpointDuration == nil {
		return
	}
	recordMicroVMPhases(ctx, i.checkpointDuration, ateattr.MicroVMCheckpointPhaseKey, phases)
}

func (i *Instruments) recordRestore(ctx context.Context, phases ...microVMPhase) {
	if i == nil || i.restoreDuration == nil {
		return
	}
	recordMicroVMPhases(ctx, i.restoreDuration, ateattr.MicroVMRestorePhaseKey, phases)
}

// recordMicroVMPhases skips zero-valued phases: those never started, and
// reporting them as instantaneous would drag every percentile down.
func recordMicroVMPhases(ctx context.Context, h metric.Float64Histogram, phaseKey attribute.Key, phases []microVMPhase) {
	for _, p := range phases {
		if p.d == 0 {
			continue
		}
		h.Record(ctx, p.d.Seconds(), metric.WithAttributes(phaseKey.String(p.name)))
	}
}
