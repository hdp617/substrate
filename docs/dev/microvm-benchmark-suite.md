<!--
Copyright 2026 Google LLC

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
-->

# Micro-VM suspend/resume and storage benchmark suite

**Status:** Phases 0–2 (dirty-size sweep scaffolding) are implemented.
Phase 3 (virtio-blk A/B) remains blocked on a runtime prototype. Patterns
beyond the size sweep (`many-small`, overwrite-rotate churn, RAM×disk
Matrix D) are listed in the matrix but not yet wired as `tests.yaml`
entries.

The suite has two jobs, in this order:

1. Measure **suspend and resume** on `ateom-microvm` well enough to say where
   time and bytes go, against the [north-star targets](../architecture.md#north-star-metrics)
   (100 ms p95 activation, 1000 wakeups/s).
2. Produce the evidence for a **storage-backend decision**: keep serving actor
   filesystems over virtio-fs, move writable state onto virtio-blk, or split
   the two.

It extends the existing Locust / boomer / glutton harness. It does not add a
second load generator.

## Why a dedicated suite

The current nightly list in [`benchmarking/automation/tests.yaml`](../../benchmarking/automation/tests.yaml)
is gVisor-first. The only `sandboxClass: microvm` entries are
`glutton_mem_1gi_microvm` and `glutton_mem_2gi_microvm`: they fill guest RAM
and walk it after resume. That answers "how does a large memory snapshot
behave on Cloud Hypervisor." It does not answer:

- how a dirty **rootfs** or **DurableDir** changes suspend/resume,
- how much of resume is `ateom` vs download vs first-touch I/O,
- what virtiofsd costs on the host while the actor is running,
- whether a virtio-blk writable disk would be faster, smaller, or denser.

Those are the questions this suite exists to close. The [roadmap](../roadmap.md)
already lists "representative workloads" and "storage and visualization for
benchmark results" as performance work; this is the micro-VM slice of that.

## What the runtime actually does today

A micro-VM actor is not one disk. Three storage paths sit next to each other,
and a suspend/resume cycle touches all of them:

| Path | Backend today | In a Full snapshot | In a Data snapshot |
|---|---|---|---|
| Kata guest OS (`/dev/vda`, read-only) | virtio-blk (`rootfs.img`) | Not shipped (content-addressed asset) | Not shipped |
| Actor container rootfs (writable) | Host overlay (OCI lower + per-actor upper) served over **one** virtio-fs share | `rootfs-upper.tar` | Discarded (cold boot) |
| `DurableDir` / CSI mounts | Subdirectories of the **same** virtio-fs share | `durable-dir.tar` | Same tar; restore is a cold boot (or golden + data) |
| Guest RAM | Cloud Hypervisor memfd, OnDemand restore | `memory-ranges` (sparse) + `config.json` / `state.json` | Not shipped |

The comments in `cmd/ateom-microvm/rootfsupper.go` record two designs that
already lost: a guest tmpfs upper (every write pinned in guest RAM, snapshot
grew with the working set) and a guest-mounted overlay on a virtio-fs upper
(three kernel workarounds). The current host overlay + virtio-fs share is
what we measure first. virtio-blk is the candidate we have **not** tried for
actor-writable state. The kata guest image stays on virtio-blk either way;
that disk is not in the decision.

Checkpoint already runs the independent pieces concurrently while the guest
is paused (`cmd/ateom-microvm/checkpoint.go`): Cloud Hypervisor snapshot,
rootfs-upper tar, durable-dir tar. The paused window is the max of those, not
the sum. Restore already overlaps the upper untar with bundle preparation
(`cmd/ateom-microvm/restore.go`). Both paths log per-phase durations. Those
logs are the raw material for the metrics below; they are not queryable
across a run today.

`atelet.snapshot.size` already records every file ateom lists, labeled by
`file.name`. The registry text still says "gVisor snapshot image"; on
micro-VM the same instrument would already distinguish `memory-ranges`,
`rootfs-upper.tar`, and `durable-dir.tar` if the tests ran. The histogram
does not carry `ate.sandbox.class`.

## Questions the suite must answer

Each question names the evidence that closes it. A run that cannot produce
that evidence is not part of the suite.

### Q1. Where does suspend time go?

Break `SuspendActor` (and `PauseActor`) into: guest pause, Cloud Hypervisor
snapshot, OnDemand delta merge, rootfs-upper tar, durable-dir tar, VMM
teardown, atelet persist (local rename vs object-store upload).

**Evidence:** ateom phase durations as metrics (today: logs only), plus
`ate.actor.checkpoint.duration` with `ate.snapshot.phase` (`ateom_checkpoint`
vs `persist`). Locust `SuspendActor` / `PauseActor` rows are the client
total, not the breakdown.

### Q2. Where does resume time go?

Break `ResumeActor` into: volume mount, manifest fetch, sandbox assets,
snapshot download, OCI unpack, ateom restore (prep, bundles, upper join,
overlay+virtiofsd, tap, VMM launch, `vm.restore`, resume, readyz), then
**first-touch** of memory and of disk after the RPC has returned.

**Evidence:** existing `ate.actor.restore.duration` phases, plus ateom restore
sub-phases promoted from the "Actor restore phases" log, plus a workload
row that walks RAM (`ReadRAM`) and a workload row that reads the files
written before suspend (`ReadDisk`). The north-star number is activation
latency: RPC return is not enough if the actor then page-faults for 200 ms
on the first request.

### Q3. How does dirty filesystem size scale the snapshot?

Hold guest RAM fixed (the 256 MiB micro-VM floor, no `WriteRAM`) and grow
writable files: 0, 8 MiB, 64 MiB, 256 MiB, 1 GiB. Repeat once against the
**rootfs upper** (`glutton --data-dir=/tmp/glutton`) and once against a
**DurableDir**.

**Evidence:** `SuspendActor` p50/p95, `rootfs-upper.tar` / `durable-dir.tar`
bytes, `persist` duration. The hypothesis to confirm or kill: tar of a
host overlay upper is linear in dirty bytes and will dominate the paused
window well before 1 GiB.

### Q4. How does dirty memory interact with dirty disk?

The existing 1 GiB / 2 GiB RAM tests, with and without a 64–256 MiB dirty
rootfs. Does virtiofsd's host page cache inflate `memory-ranges`? Does the
concurrent tar lengthen the paused window past the Cloud Hypervisor
snapshot?

**Evidence:** `memory-ranges` size with and without a dirty rootfs of the
same byte count; `snapshot` vs `rootfs_upper` durations on the same run.

### Q5. What does runtime I/O cost on virtio-fs, before we build virtio-blk?

Sequential write, sequential read, overwrite-in-place, many small files,
and (once glutton grows a flag) `fsync` / `O_DIRECT`. Measured **while the
actor is running**, not across a suspend.

**Evidence:** Locust `WriteDisk` / `ReadDisk` latency and throughput.
Host-side: virtiofsd CPU and RSS, worker-pod disk I/O, guest CPU. This is
the baseline a virtio-blk prototype has to beat on the **hot path**,
independent of snapshot cost.

### Q6. Pause versus suspend

Pause keeps the snapshot on the node (`persist` is a rename). Suspend
uploads it. The storage backend changes both, but not equally: a faster
local disk snapshot helps pause a lot and suspend only until the upload
starts.

**Evidence:** the same workload with `--lifecycle-mode pause` and
`--lifecycle-mode suspend`. Compare `ateom_checkpoint` (should be similar)
against `persist` (should not).

### Q7. Explicit versus implicit resume

The DurDir harness already has `--resume-mode explicit|implicit`. Implicit
resume is the product path (router parks the request, ateapi resumes). The
north-star 100 ms is this number, not the explicit RPC.

**Evidence:** DurDir-style cycle with `--resume-mode implicit` on micro-VM,
reporting the request's end-to-end time (`DurDirServeAfterResume` or the
glutton ping that caused the wake).

### Q8. virtio-fs versus virtio-blk (the decision)

Once a virtio-blk writable path exists, rerun Q3, Q4, Q5, and Q6 against
both backends, same cluster, same actor sizes. See
[Decision criteria](#decision-criteria).

Until that path exists, the suite still runs: every question except Q8 is
answerable on today's virtio-fs runtime, and the workload knobs must not
assume a backend.

## Design principles

1. **Reuse the harness.** Locust master + boomer-Go worker + glutton actor +
   `benchmarking/automation` CronJob. A new generator would fork the
   telemetry, the result upload, and the cluster lifecycle.
2. **One user class, many templates.** Disk work already lives in glutton
   (`WriteDisk` / `ReadDisk`). Point `--data-dir` at `/tmp/glutton` for
   rootfs-upper, or at a DurableDir mount for volume-backed data. Do not
   invent a second disk server.
3. **Backend-agnostic load.** The actor writes files. The sandbox class and
   (later) a storage-backend knob on the WorkerPool / SandboxConfig choose
   virtio-fs or virtio-blk. The locust file does not branch on backend.
4. **Composition, not only totals.** A 400 ms `ResumeActor` that is 350 ms
   download and 50 ms ateom is a different bug from the reverse. Promote
   ateom's existing phase logs to metrics; do not add a parallel stopwatch
   in the load generator.
5. **gVisor as a control, not a target.** A subset of the matrix runs on
   gVisor so a micro-VM regression is visible as a delta against a known
   class, not as an absolute number with no baseline.
6. **Fixed hardware.** micro-VM tests require KVM-capable nodes (already
   documented in `benchmarking/automation/README.md`). Do not mix nested-virt
   kind numbers with GCE nested-virt numbers in the same table.

## Workload

### What glutton already provides

- `WriteRAM` / `ReadRAM` with fill, rotate-churn, and a one-byte-per-page
  walk (the existing large-memory tests).
- `WriteDisk` / `ReadDisk` with truncate or overwrite, SHA-256, and a
  digest-only read mode so large files do not return over the router.
- DurDir user class: write once, suspend, resume, read-after-resume,
  warm read, overwrite. Templates `glutton-durdir-data` (Data-scope commit,
  cold boot) and `glutton-durdir-full` (Full-scope commit).

The default `glutton` template already writes under `/tmp/glutton`, which on
micro-VM **is** the host overlay upper. Nobody currently drives `WriteDisk`
against that template; DurDir is the only disk cycle, and it only runs on
gVisor in `tests.yaml`.

### Gaps to close in glutton / boomer

These are small, local extensions, not a new workload:

| Gap | Why it matters |
|---|---|
| `WriteDisk.size` is `int32` | A 1 GiB dirty rootfs does not fit. Use `int64` (or the same suffixed string `WriteRAM` already takes). |
| Sequential-only writes | virtio-fs vs virtio-blk often diverges on random I/O and metadata, not on a single streaming write. Add a write pattern: sequential, overwrite-rotate (already in RAM), N files of size S, optional `fsync` / `O_DIRECT`. |
| No "disk churn" analog of `--mem-churn` | Repeated suspends of a static file measure tar of an unchanged upper. Rotate a window of the file each cycle so the upper actually changes. |
| No first-touch disk row on the `glutton` user class | DurDir has `DurDirServeAfterResume`. The rootfs cycle needs the same: read the file immediately after resume, before a warm read. |
| No backend / target label on Locust rows | Rows should carry `rootfs` vs `durdir` (from the template) so a combined dashboard does not mix them. The backend (virtiofs vs virtio-blk) is an automation dimension, not a client flag. |

A reasonable shape: keep `GluttonUser` for memory-only cycles, keep
`DurdirUser` for DurableDir, add a thin `DiskUser` (or a mode on DurDir)
that targets the default `glutton` template's `/tmp` rootfs. Do not collapse
all three into one class; the existing tests.yaml entries should keep
working.

### I/O patterns to ship

Named so `tests.yaml` can select them with flags, like `--mem-target`:

- `seq-write` — one file, truncate, streaming write (today's DurDir).
- `seq-read` — digest-only read of that file.
- `overwrite-rotate` — advancing window, same as RAM churn.
- `many-small` — N files of 4 KiB (metadata / dentry stress on virtio-fs).
- `fsync` — `seq-write` then `fsync` (durability; virtio-fs write-through
  vs virtio-blk cache modes).

Not in v1: fio-style mixed random read/write inside the guest. If Q5 on
`seq-write` / `many-small` / `fsync` does not separate the backends, add it
then. Do not start there.

## Metrics

Follow [metrics best practices](best-practices/metrics.md): fleet histograms
with bounded labels; per-actor breakdowns stay on the structured log ateom
already emits.

### Promote ateom phases (required for Q1 and Q2)

Today these are log fields only.

**Checkpoint** (`cmd/ateom-microvm/checkpoint.go`): `pause`, `snapshot`
(Cloud Hypervisor + merge), `durable_dir`, `rootfs_upper`, `teardown`.

**Restore** (`cmd/ateom-microvm/restore.go`): `prep`, `bundles`,
`upper_join`, `lowers` (overlay mount + virtiofsd), `tap`, `vmm_launch`,
`vm_restore`, `resume`, `readyz`.

Do **not** explode `ate.snapshot.phase` with all of these. That attribute is
atelet's coarse handoff (`ateom_checkpoint`, `persist`, `download`, …). Add
a second instrument on ateom, labeled by a new bounded attribute
(for example `ate.microvm.checkpoint.phase` / `ate.microvm.restore.phase`),
emitted only by `ateom-microvm`. The locust totals stay on
`ate.actor.lifecycle.operation.duration`.

The merge of an OnDemand delta into `memory-ranges` is part of `snapshot`
today and can hide a large copy. Split it out (`merge`) on the ateom
histogram; Q4 will otherwise blame Cloud Hypervisor for ateom's overlay.

### Snapshot composition (required for Q3 and Q8)

Keep `atelet.snapshot.size` (`file.name` = `memory-ranges` |
`rootfs-upper.tar` | `durable-dir.tar` | …). Update the registry text so it
is not gVisor-only, and add `ate.sandbox.class` so a dashboard can split
classes without enumerating templates.

Report **logical** size (what `stat` returns) and, for `memory-ranges`, the
sparse apparent size if it differs. A sparse 1 GiB image that stores 80 MiB
is the whole point of OnDemand.

### Host cost of the backend (required for Q5 and Q8)

virtiofsd is a host process in the worker pod cgroup (the
`--vmm-mem-reserve-mib` margin exists because of it). virtio-blk moves that
work into Cloud Hypervisor's disk I/O threads.

Measure, per worker pod, during the running phase of each test:

- virtiofsd CPU and RSS (0 on a virtio-blk-only writable path),
- Cloud Hypervisor CPU,
- pod cgroup memory and disk I/O (cAdvisor already scraped in
  `benchmarking/monitoring.yaml`).

Do not put actor identity on these series. Template + sandbox class +
backend is enough.

### Locust rows (client view)

Minimum set for a disk cycle, mirroring DurDir:

| Row | Meaning |
|---|---|
| `WriteDisk` / `DurDirWrite` | Runtime write before the first suspend |
| `SuspendActor` or `PauseActor` | Client-observed hibernate |
| `ResumeActor` or implicit request time | Client-observed activation |
| `ReadDiskAfterResume` / `DurDirServeAfterResume` | First-touch of the previous cycle's files |
| `ReadDiskWarm` / `DurDirServeWarm` | Cached read in the same activation |
| `ReadRAM` | First-touch of guest memory (when `--mem-read` is on) |

## Experiment matrix

Every cell is one `tests.yaml` entry (or one ladder step). Names follow the
existing `glutton_*` / `durdir_*` pattern. Default duration 1 minute, 1
user, 1 worker, unless the row is about concurrency.

### A. Suspend/resume baseline (virtio-fs, empty disk)

| Test | Class | Memory | Disk | Lifecycle | Why |
|---|---|---|---|---|---|
| `microvm_glutton_baseline` | microvm | floor (256 MiB) | none | suspend | Empty-actor activation floor |
| `microvm_glutton_baseline_pause` | microvm | floor | none | pause | Pause vs suspend on the same actor |
| `microvm_glutton_baseline_implicit` | microvm | floor | none | suspend, implicit resume | North-star path |
| `gvisor_glutton_baseline` | gvisor | same | none | suspend | Control (already exists as `glutton_baseline_1_user`) |

The existing `glutton_baseline_*` entries stay; the new ones are the
micro-VM copies plus implicit resume.

### B. Memory (already present; keep)

`glutton_mem_1gi_microvm`, `glutton_mem_2gi_microvm`. Add pause variants
(`glutton_mem_*_gvisor` already has them; micro-VM does not). Add
`--mem-read all` is already on.

### C. Dirty filesystem, Full snapshot

| Test | Target | Size | Pattern |
|---|---|---|---|
| `microvm_rootfs_8m` / `_64m` / `_256m` / `_1g` | rootfs upper | as named | seq-write, digest read |
| `microvm_durdir_full_8m` / `_64m` / `_256m` / `_1g` | DurableDir, Full commit | as named | same |
| `microvm_rootfs_many_small` | rootfs upper | 16k × 4 KiB | many-small |
| `microvm_rootfs_8m_pause` | rootfs 8 MiB | pause | pause vs suspend at one size |
| `microvm_rootfs_64m_churn` | rootfs 64 MiB | overwrite-rotate 8 MiB/cycle | changing upper |

gVisor controls: the existing `durdir_size_*` entries, plus one rootfs
gVisor run at 64 MiB so the overlay-upper tar has a sibling.

### D. Memory × disk

| Test | RAM | Disk |
|---|---|---|
| `microvm_mem_1gi_rootfs_64m` | 1 GiB fill + 64 MiB churn | 64 MiB rootfs |
| `microvm_mem_1gi_durdir_64m` | 1 GiB | 64 MiB DurableDir |

If `memory-ranges` grows with the rootfs dirty set, virtio-fs page cache is
leaking into the guest snapshot and Q8 must count that as a virtio-fs cost.

### E. Concurrency / density (after A–D are stable)

| Test | Users | Workers | Why |
|---|---|---|---|
| `microvm_glutton_5` / `_10` | 5 / 10 | 5 / 10 | Same as gVisor baseline, on KVM nodes |
| `microvm_oversub_15` | 15 | 10 | Scheduler queue vs snapshot cost |
| `microvm_rootfs_64m_5` | 5 | 5 | Five concurrent virtiofsd + tars |

Do not start here. A 15-user run that we cannot attribute is noise.

### F. virtio-blk A/B (blocked on a prototype)

Repeat C, D, and the 64 MiB row of E with a backend label:

- `storageBackend: virtiofs` (default, today's code)
- `storageBackend: virtio-blk` (writable upper and/or DurableDir as a
  Cloud Hypervisor disk)

Same `targetCluster`, same `actorMemory`, same flags. The only independent
variable is the backend. If the prototype only covers DurableDir, run the
`durdir_*` rows first and leave rootfs on virtio-fs; do not pretend a
partial prototype is a full comparison.

## Decision criteria

The suite does not pick a winner by a single latency. Score each backend on
the axes below, using the matching experiments. A backend that wins
runtime I/O and loses suspend by 2× is still a candidate if pause (local
persist) is the common path; it is not a candidate if every idle actor
suspends to object storage.

| Axis | Winner if | Measured by |
|---|---|---|
| Activation p95 (explicit) | Closer to 100 ms on Full resume of a 64 MiB dirty + 256 MiB RAM actor | Q2, matrix C+D |
| Activation p95 (implicit) | Same, through the router | Q7 |
| Suspend p95, Full | Smaller paused window at 64 MiB and 256 MiB dirty | Q1, Q3 |
| Suspend p95, Pause | Local persist stays cheap as dirty bytes grow | Q6 |
| Snapshot bytes | Smaller `rootfs-upper` / disk image + no inflation of `memory-ranges` | Q3, Q4 |
| Runtime write | Higher throughput / lower p95 on `seq-write` and `many-small` | Q5 |
| Runtime fsync | Lower p95 if agents `fsync` working files | Q5 `fsync` |
| Host density | More concurrent actors per node before virtiofsd/VMM CPU or pod memory saturates | matrix E |
| Operational complexity | Fewer restore-path special cases (find-paths, overlay whiteouts, per-volume devices) | qualitative, after numbers |

Expected tensions, so the write-up does not treat them as surprises:

- virtio-fs keeps **many DurableDirs free** (subdirs of one share) and is
  how system-info is rewritten live (`cmd/ateom-microvm/systeminfo.go`).
  virtio-blk costs a device per volume and does not give find-paths.
- virtio-fs write-through means a paused guest's completed writes are
  already on the host; the tar is coherent without a guest freeze-fs. A
  virtio-blk disk may need a guest `fsync` / cache flush before snapshot,
  which shows up in Q1 `pause` + Q5 `fsync`.
- A qcow2 / raw disk snapshot can be faster than a tar walk at large
  sequential dirty sets and slower at `many-small` (block allocation vs
  file tar). Matrix C includes both shapes for that reason.
- OnDemand memory restore is independent of the disk backend. Do not credit
  virtio-blk for a `vm_restore` improvement it did not cause.

**Default recommendation rule:** keep virtio-fs if it meets the 100 ms p95
activation target on the 64 MiB dirty Full-resume actor and its host CPU
does not cap density below the gVisor control. Move writable state to
virtio-blk only if Q3/Q5 show a clear suspend or runtime win **and** the
system-info / multi-DurableDir constraints still have a design (hybrid:
virtio-fs for the RO lower + system-info, virtio-blk for the writable
upper and/or DurableDir, is in scope for the prototype; all-blk is not
required to run F).

## Implementation phases

Each phase is a PR-sized slice. Later phases consume earlier metrics; do
not skip instrumentation.

### Phase 0 — Make today's runtime observable

No new tests.yaml entries required.

- Emit ateom checkpoint/restore sub-phase histograms (see
  [Metrics](#metrics)).
- Label `atelet.snapshot.size` with `ate.sandbox.class`; fix the registry
  blurb.
- Optionally split OnDemand `merge` out of `snapshot` in the ateom log and
  histogram.

Exit: a single manual `glutton_mem_1gi_microvm` run produces a phase table
and a per-file snapshot size table without grepping logs.

### Phase 1 — micro-VM parity on the existing cycles

- Add `sandboxClass: microvm` copies of `glutton_baseline_1_user`, its
  pause variant, and `durdir_full_baseline` / `durdir_data_baseline`.
- Add a rootfs-disk cycle against the default `glutton` template
  (`DiskUser` or DurDir pointed at `/tmp`), 8 MiB, suspend + first-touch
  read.
- Widen `WriteDisk` size so 64 MiB+ runs are possible.

Exit: nightly (or on-demand) micro-VM numbers for empty-actor
suspend/resume, DurableDir Full vs Data, and an 8 MiB rootfs Full cycle.

### Phase 2 — Scale the dirty set and the patterns

- Matrix C size sweep (8 / 64 / 256 / 1 GiB) for rootfs and DurableDir.
- `many-small`, overwrite-rotate churn, pause vs suspend at one size.
- Matrix D (1 GiB RAM × 64 MiB disk).
- Implicit resume on one disk cycle (Q7).

Exit: a plot of suspend time and snapshot bytes against dirty filesystem
size, with RAM held fixed, on virtio-fs. This is enough to decide whether
a virtio-blk prototype is worth building.

### Phase 3 — virtio-blk prototype A/B

Blocked on a runtime flag that can attach a writable virtio-blk disk for
the rootfs upper and/or DurableDir without changing the locust files.

- Automation dimension `storageBackend`.
- Repeat C, D, and the 64 MiB density row.
- Write the comparison as a short note next to this document (results live
  in GCS, like the observability ladder; do not check numbers into git).

Exit: a go / no-go on virtio-blk for writable actor state, using
[Decision criteria](#decision-criteria).

### Phase 4 — Density and soak

- Matrix E on the winning backend (and on virtio-fs if the decision was
  hybrid).
- A 10-minute soak at the 64 MiB dirty size to catch virtiofsd leaks and
  snapshot-merge drift.

## What this suite is not

- **CSI backends.** CSI mounts already ride the virtio-fs share;
  Data-scope snapshots currently ignore them
  (`cmd/ateom-microvm/checkpoint.go`). Out of scope until that snapshot
  path exists.
- **The kata guest image disk.** It is already virtio-blk and read-only.
  Cold-boot reads from `/dev/vda` were optimized by booting the agent as
  PID 1; that is a different knob.
- **Network / ingress capacity.** `nighthawk-ingress` already covers the
  router. Do not mix Envoy CPU into a storage decision.
- **gVisor snapshot format work.** The gVisor rows are controls.
- **Checking results into the repo.** Follow
  [`benchmarking/observability.md`](../../benchmarking/observability.md):
  automation writes GCS; a number in git goes stale the next time the
  runtime changes.

## Open questions

These do not block Phase 0–2. They block a Phase 3 prototype.

1. **Hybrid or all-writable-blk?** System-info live rewrite and find-paths
   want a virtio-fs share for the RO lower. A prototype that only replaces
   the writable upper (and DurableDir) with virtio-blk is enough to run
   matrix F and is closer to a shippable design than replacing the share
   entirely.
2. **Disk image format.** raw vs qcow2 vs Cloud Hypervisor's snapshot of
   the disk. The suite should treat "the bytes we persist for the writable
   disk" as one `file.name` and not care about the format, but the
   prototype must pick one before F runs.
3. **Where the backend knob lives.** WorkerPool, SandboxConfig, or
   ActorTemplate. The suite only needs a stable label on the run; the API
   debate is separate.
4. **OnDemand vs Eager as a test dimension.** Restore memory mode follows
   the VMM (`restoreMemMode`). Do not add a client flag unless we grow a
   runtime override; mixing modes in one table without labeling them
   invalidates Q2.

## Related code

| Piece | Path |
|---|---|
| Existing suite | [`benchmarking/README.md`](../../benchmarking/README.md) |
| Nightly list | [`benchmarking/automation/tests.yaml`](../../benchmarking/automation/tests.yaml) |
| Glutton actor | [`internal/benchmarking/glutton`](../../internal/benchmarking/glutton) |
| Boomer cycles | [`internal/benchmarking/boomer/glutton`](../../internal/benchmarking/boomer/glutton) |
| micro-VM checkpoint | [`cmd/ateom-microvm/checkpoint.go`](../../cmd/ateom-microvm/checkpoint.go) |
| micro-VM restore | [`cmd/ateom-microvm/restore.go`](../../cmd/ateom-microvm/restore.go) |
| Host overlay upper | [`cmd/ateom-microvm/rootfsupper.go`](../../cmd/ateom-microvm/rootfsupper.go) |
| virtio-fs share | [`cmd/ateom-microvm/internal/kata/overlay_linux.go`](../../cmd/ateom-microvm/internal/kata/overlay_linux.go) |
| CH disk/fs config | [`cmd/ateom-microvm/internal/ch/createvm.go`](../../cmd/ateom-microvm/internal/ch/createvm.go) |
| Snapshot size metric | [`cmd/atelet/main.go`](../../cmd/atelet/main.go) (`recordSnapshotSize`) |
| Metric registry | [`docs/metrics/registry/metrics.yaml`](../metrics/registry/metrics.yaml) |
