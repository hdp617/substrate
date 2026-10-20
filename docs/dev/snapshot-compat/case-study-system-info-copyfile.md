# Case study: rolling out a breaking micro-VM snapshot change with a compatibility key

*2026-10-20T01:51:41Z by Showboat 0.6.1*
<!-- showboat-id: 3466d94f-5c69-4440-8673-0965798af411 -->

This case study tests Substrate's snapshot compatibility mechanism (Actor Lifecycle v2) against a **real** breaking data-plane change, on a **real** GKE cluster with nested-virtualization micro-VM workers.

**Question:** can today's mechanism (the `SandboxRuntime` stamp on each snapshot, and the `AttributeEntry` matching in `internal/hardware`) roll out a change that alters what a memory snapshot's guest depends on? Can it do so progressively, pool by pool, and roll it back without losing or corrupting actors?

**Method:**

1. Implement a real breaking change in `ateworker-microvm`.
2. Show on the cluster how it breaks snapshots with no compatibility support (the **naive** build, experiment E1).
3. Add compatibility support: a worker-reported attribute, plus guest-shape inheritance in the control plane.
4. Repeat the upgrade, a mixed fleet and a rollback with it (experiment E2).

Code and branch history below are live `showboat exec` blocks. Cluster results are quoted as captured, since those runs cannot be replayed. All runs were on 2026-10-20 between 01:20 and 02:30 UTC.

## 1. The change: system-info volumes over kata `CopyFile`

Micro-VM *system-info volumes* (`actorMetadata`, `trustBundle`) are per-actor files atelet writes on the host. Until now the worker bind-mounted them into the guest's virtio-fs share. The new mode copies them into guest memory with the kata agent's `CopyFile` RPC.

The kata 4.1.0 agent only writes `CopyFile` targets beneath `/run/kata-containers/shared/containers/`, which is the very directory Substrate mounts the virtio-fs share on. So the change mounts a guest tmpfs **inside** the share when the sandbox is created:

```bash
sed -n '/^func sandboxStorages/,/^}/p' cmd/ateworker-microvm/internal/kata/overlay_linux.go
```

```output
func sandboxStorages(opts CreateSandboxOpts) ([]*agentpb.Storage, error) {
	storages := []*agentpb.Storage{{
		Driver:     virtioFSDriver,
		Source:     FsTag,
		Fstype:     typeVirtioFS,
		MountPoint: guestSharedDir,
	}}
	if opts.TmpfsDir == "" {
		return storages, nil
	}
	if !filepath.IsLocal(opts.TmpfsDir) {
		return nil, fmt.Errorf("guest tmpfs dir %q is not a path under the share", opts.TmpfsDir)
	}
	// The agent's ephemeral driver creates the mount point and mounts a fresh
	// tmpfs there.
	return append(storages, &agentpb.Storage{
		Driver:     ephemeralDriver,
		Source:     typeTmpfs,
		Fstype:     typeTmpfs,
		MountPoint: guestSharedDir + filepath.Clean(opts.TmpfsDir),
	}), nil
}
```

The worker pushes every file after the sandbox exists and before containers start, again after every MEMORY restore, and on every change atelet makes (a shared fsnotify watcher with a periodic rescan).

Two consequences make this change **breaking for MEMORY snapshots in both directions**:

- A guest booted by the old build has its containers bound to `…/system-info` on virtio-fs. A new build that no longer stages that directory leaves the frozen guest holding paths that resolve nowhere.
- A guest booted by the new build has its files in guest RAM. An old build restoring it never refreshes them, so a clone of a shared snapshot keeps the source actor's identity.

## 2. The compatibility support

**Self-describing data plane.** A MEMORY checkpoint records the guest's delivery in a `system-info-delivery` file. A snapshot without one predates the change. A restore keeps the delivery the guest booted with: a virtio-fs guest gets today's bind and no copies.

```bash
sed -n '/^func readSystemInfoDelivery/,/^}/p' cmd/ateworker-microvm/systeminfo.go
```

```output
func readSystemInfoDelivery(snapshotDir string) (ocispec.SystemInfoDelivery, error) {
	b, err := os.ReadFile(filepath.Join(snapshotDir, systemInfoDeliveryFile))
	if errors.Is(err, fs.ErrNotExist) {
		return ocispec.SystemInfoVirtioFS, nil
	}
	if err != nil {
		return 0, fmt.Errorf("reading %s: %w", systemInfoDeliveryFile, err)
	}
	switch v := string(b); v {
	case systemInfoCopyFile:
		return ocispec.SystemInfoCopyFile, nil
	case systemInfoVirtioFS:
		return ocispec.SystemInfoVirtioFS, nil
	default:
		return 0, fmt.Errorf("snapshot has unknown system-info delivery %q", v)
	}
}
```

**A worker-reported compatibility attribute.** The worker binary, not atelet, decides the guest's shape, so it reports `system_info_delivery=copyfile` on every runtime it registers (`register.ReportConfig.ExtraAttributes`). Matching is unchanged: a snapshot stamped `copyfile` only matches workers that report it, and keys only the worker has are ignored.

```bash
grep -n 'AttrSystemInfoDelivery\|WorkerAttributes\|guestShapeAttributes' internal/hardware/hardware.go cmd/ateworker-microvm/main.go
```

```output
internal/hardware/hardware.go:55:	// AttrSystemInfoDelivery is how a micro-VM guest receives its system-info
internal/hardware/hardware.go:59:	AttrSystemInfoDelivery = "system_info_delivery"
internal/hardware/hardware.go:62:// guestShapeAttributes are the attributes that describe what a guest was
internal/hardware/hardware.go:65:var guestShapeAttributes = []string{AttrSystemInfoDelivery}
internal/hardware/hardware.go:67:// WorkerAttributes are the attributes a worker binary reports itself, rather
internal/hardware/hardware.go:69:var WorkerAttributes = []string{AttrSystemInfoDelivery}
internal/hardware/hardware.go:241:		return slices.Contains(guestShapeAttributes, a.GetKey()) && !r.hasAttribute(a.GetKey())
cmd/ateworker-microvm/main.go:409:	return map[string]string{hardware.AttrSystemInfoDelivery: systemInfoCopyFile}
```

**Guest-shape inheritance in the control plane.** The snapshot stamp is copied from the worker assignment. Without a fix, an old virtio-fs guest restored on a new worker would be stamped `copyfile` at its next suspend and then could never go back to an old worker. `RuntimeFor` therefore drops guest-shape attributes the restored snapshot doesn't carry:

```bash
sed -n '/^func (r Requirement) RuntimeFor/,/^}/p;/^func (r Requirement) inheritGuestShape/,/^}/p' internal/hardware/hardware.go
```

```output
func (r Requirement) RuntimeFor(status *ateapipb.WorkerStatus) *ateapipb.SandboxRuntime {
	rt := r.MatchingRuntime(status)
	if rt == nil {
		return status.GetDefaultRuntime()
	}
	if r.snap == nil {
		return rt
	}
	return r.inheritGuestShape(rt)
}
func (r Requirement) inheritGuestShape(rt *ateapipb.SandboxRuntime) *ateapipb.SandboxRuntime {
	drop := func(a *ateapipb.AttributeEntry) bool {
		return slices.Contains(guestShapeAttributes, a.GetKey()) && !r.hasAttribute(a.GetKey())
	}
	if !slices.ContainsFunc(rt.GetVersion().GetAttributes(), drop) {
		return rt
	}
	out := proto.CloneOf(rt)
	// Deleting keeps the remaining attributes in their order, which is sorted
	// by key, each key once.
	out.Version.Attributes = slices.DeleteFunc(out.Version.Attributes, drop)
	return out
}
```

**Release split.** The version-skew policy upgrades workers *before* ate-api-server. The inheritance fix lives in ate-api-server, so it must ship a release before the worker that reports the key:

- **N+1a:** hardware inheritance plus register `ExtraAttributes`. Inert, because no worker sets the key.
- **N+1b:** the worker that delivers by CopyFile and advertises the key.

The branch, with the naive build at `d54eeff2` (built for the runs as `92243c84`, before a rebase):

```bash
git log --format='%h %s' 9e921657..HEAD | cat
```

```output
f5267b46 ateworker-microvm: list the delivery marker among MEMORY snapshot files
c7e47e85 ateworker-microvm: log a timed-out system-info sync once
0f8532ac ateworker-microvm: fail a system-info push cut short by its context
d1332283 e2e: compare a golden clone's assignment with RuntimeFor
b2749507 hardware, controlapi: test that CopyFile snapshots stay off legacy workers
febc2f54 ateworker-microvm: cancel system-info pushes on stop and keep .tmp siblings
29ac10c7 ateworker-microvm: require an explicit system-info delivery
d0369111 register: reject an empty extra attribute value
0fc6709f ateworker-microvm: keep syncing system-info after a failed pause
509ffac4 docs: describe micro-VM system-info copies and guest-shape attributes
31b6ba4c ateworker-microvm: keep snapshots' system-info delivery and advertise it
d54eeff2 ateworker-microvm: deliver system-info volumes with CopyFile
6da73956 register: let a worker report attributes of its own
2309e249 hardware: inherit guest-shape attributes when a snapshot's runtime matches
9f8c18b7 ocispec: add a CopyFile delivery for micro-VM system-info volumes
4963bc41 kata: add CopyFile and a guest tmpfs sandbox storage
```

## 3. Environment

- **Cluster:** GKE `substrate-poc` (`huypham-gk8s-dev`, `us-west1-c`), node pool `substrate-node-pool`: 2× `c3-standard-4` with nested virtualization, COS.
- **Release N:** upstream main `0fdb67b5`, deployed with `ate-setup deploy ate-system`.
- **Runtime:** micro-VM SandboxConfig `microvm` v1 (kata 4.1.0, cloud-hypervisor), the same for every worker build.
- **Workload:** the e2e probe fixture (`internal/e2e/fixtures/probe`). Its `/whoami` reports what the actor reads from its system-info volume at `/run/ate`, including through a file descriptor opened at startup (`held`).
- **Memory witness:** `/writefile?path=/dev/shm/witness` puts a file in guest RAM. It survives only a MEMORY restore, so its presence after a resume tells MEMORY from ROOTFS.
- **Golden snapshots:** disabled on the case-study templates. The cluster's template reconciler fails on a template stored by an earlier deployment (finding F7), so shared snapshots come from explicit tags instead, which is the same mechanism a golden snapshot uses.

The images and the commits they were built from. The runs predate a rebase of the branch onto `9e921657`; the last column gives each commit's rebased equivalent.

| Image | Digest (prefix) | Built from | Rebased as |
|---|---|---|---|
| probe | `probe-cb12…@sha256:b310912da79c` | `0fdb67b5` (upstream main) | unchanged |
| worker N | `ateworker-microvm@sha256:06d5eb19b8df` | `0fdb67b5` | unchanged |
| worker naive | `ateworker-microvm@sha256:eab2ef85d018` | `92243c84` | `d54eeff2` |
| worker N+1b | `ateworker-microvm@sha256:bf9203d0af85` | `9ff3e107` | `d0369111` |
| atelet and ate-api-server N+1 | (built by `ate-setup deploy atelet` and `deploy apiserver`) | `9ff3e107` | `d0369111` |

All three workers use the same SandboxConfig, so they share one `sandbox_bundle_hash`. The fixes made in review after the runs (from `29ac10c7` on) change worker behavior only on error paths and in the syncer, and none of them was exercised on the cluster.

## 4. Phase 1 spike: does CopyFile work in a real guest? Yes

On the N+1b build, a cold boot plus a same-image MEMORY suspend and resume (captured):

    worker registered: system_info_delivery=copyfile  (through atelet N and ate-api-server N, unchanged)
    cold boot:   {"file":"spike-1","held":"spike-1","uid":"f3a7f92d-…"}
    guest mount: 63 62 0:30 /system-info /run/ate ro,relatime - tmpfs tmpfs rw
    suspend:     snapshot gen 1 MEMORY | durable: COMPLETED | stamp: …,system_info_delivery=copyfile
    uploaded:    base-id.zstd config.json.zstd manifest.json memory-ranges.zstd rootfs-upper-probe.tar.zstd state.json.zstd system-info-delivery.zstd
    resume:      witness "written by probe" (MEMORY restore); {"file":"spike-1","held":"spike-1"}
    worker staging after restore: …/shared/system-info-copy ro  (copyfile mode, from the marker)

This settled the plan's unknowns: the kata agent's policy accepts `CopyFile` and the ephemeral storage, and the agent's path-safety library writes across into the tmpfs mounted inside the share.

## 5. E1: the naive build (CopyFile, no compatibility support)

The naive build (`92243c84`, rebased as `d54eeff2`) always delivers by CopyFile, writes no marker and advertises no key. Its snapshots look exactly like N's to the scheduler.

**E1a, upgrade.** Actor `e1a` was booted and sync-suspended on N (a virtio-fs guest), then `pool-a` was rolled to the naive build and `e1a` resumed (captured):

    placement:  pool-a (naive), assignment: sandbox_bundle_hash only. Nothing stops it.
    witness:    "written by probe"  (MEMORY restore succeeded)
    whoami:     {"file":"","held":"","uid":"","error":"open /run/ate/actor-id: input/output error; open /run/ate/atespace: input/output error; open /run/ate/actor-uid: input/output error; reading held identity fd: read /run/ate/actor-id: input/output error; …"}
    state:      ACTOR_STATE_RUNNING. No component reported an error.

**E1b, rollback.** On the naive build, `e1src` was cold-booted (a CopyFile guest), sync-suspended and tagged `t1`. Then `pool-a` was rolled back to N, and a clone `e1clone` of `t1` was created and resumed (captured):

    e1clone uid: a7a86b8c-…
    whoami:      {"file":"e1src","held":"e1src","uid":"d30d7393-…"}   ← the SOURCE actor's identity
    state:       ACTOR_STATE_RUNNING, stamp: sandbox_bundle_hash only. No component reported an error.

E1b is the worst outcome in the study: an actor silently assumes another actor's identity, which is exactly what system-info volumes exist to prevent. Both failures pass every check the control plane makes, because the change is invisible to the compatibility keys.

## 6. E2: with the compatibility key, rolled in policy order

E1's actors, tags and template were deleted first: their snapshots hold CopyFile guests with no stamp and no marker. Then, with the whole cluster on N, two virtio-fs snapshots were taken: `e2old` and `e2neg`, both MEMORY, stamped with the bundle hash only.

**Negative control: workers N+1b, ate-api-server still N.** atelet was rolled to N+1 (step 1), then `pool-a` to N+1b (step 2). `e2neg` was resumed, then suspended (captured):

    resume:   assignment: …,system_info_delivery=copyfile   (ate-api-server N has no inheritance)
    witness:  "written by probe"; whoami {"file":"e2neg","held":"e2neg"}   (legacy mode restored it correctly)
    suspend:  snapshot gen 2 | stamp: …,system_info_delivery=copyfile   ← wrong: the guest is still virtio-fs

The data plane stays truthful (its marker says `virtiofs`); only the control-plane stamp is wrong. It is over-restrictive, not unsafe. Section E2c shows what it costs.

**E2a, upgrade.** ate-api-server was rolled to N+1 (step 3), then `e2old` was resumed on N+1b (captured):

    assignment:     sandbox_bundle_hash only   (inherited: the snapshot lacks the key)
    witness:        "written by probe" (MEMORY restore); whoami {"file":"e2old","held":"e2old"}
    worker staging: …/shared/system-info ro    (legacy virtio-fs mode, chosen by the absent marker)
    suspend:        snapshot gen 2 | stamp: sandbox_bundle_hash only   (still restorable on N)

**E2b, mixed fleet.** `e2s` was cold-booted on N+1b (stamp `copyfile`), then `pool-old` was added on the N image (captured):

    e2s, resumed 5 times:   pool-a, pool-a, pool-a, pool-a, pool-a    (never the old pool)
    e2old, resumed 6 times: pool-old, pool-a, pool-a, pool-a, pool-a, pool-old   (witness intact every time)

With `pool-a` scaled to zero, `e2s` resumed again (captured):

    placement: pool-old (N), assignment: sandbox_bundle_hash only
    witness:   open /dev/shm/witness: no such file or directory   (ROOTFS: memory given up, as designed)
    whoami:    {"file":"e2s","held":"e2s","uid":"720c488d-…"}   (its own identity)
    ateapi:    WARN "Assigned worker cannot restore the snapshot's runtime; restoring without memory state"
               actor=compat-case-study/e2s required_runtime=…system_info_delivery=copyfile

After that ROOTFS restore on N, `e2s`'s next snapshot was stamped without the key, so the actor had migrated back to a legacy guest.

**E2c, rollback.** Before the rollback, `e2t` was cold-booted on N+1b (a CopyFile guest, with a witness), sync-suspended and tagged `t2`. Then the rollback ran in the documented order: ate-api-server to N, workers to N, atelet to N. The control-plane digests came back identical to release N. After that (captured):

    e2c (clone of t2, uid d4669e08-…): pool-a (N) | whoami {"file":"e2c","held":"e2c"} | witness: absent (ROOTFS) | fallback logged
    e2old (stamped without key):       pool-a (N) | witness "written by probe" (MEMORY)
    e2neg (stamped copyfile wrongly):  pool-a (N) | witness: absent (ROOTFS) | fallback logged

- **e2c:** where the naive build leaked the source's identity (E1b), the compat build degrades to ROOTFS with the clone's own identity.
- **e2old:** inheritance at E2a is what kept its memory restorable after the rollback.
- **e2neg:** the negative control's wrong stamp costs exactly that memory. This is why the release split puts the control plane first.

**E2d, paused snapshot.** Before the rollback, `e2p` was async-suspended on N+1b (a node-local snapshot on `…-1j57`, durable copy PENDING, stamp `copyfile`). After the rollback (captured):

    resume:          Error: rpc error: code = ResourceExhausted desc = no worker has room for the actor
    worker on 1j57:  pool-a (N), 0/1000 actors   ← room was not the problem
    ~30 min later:   locality cleared, durable COMPLETED, assignedNode unset   (atelet N detached it)
    resume:          pool-a (N) | fallback logged | whoami {"file":"e2p","held":"e2p","uid":"d873d72d-…"} | witness: absent (ROOTFS)

A paused snapshot is never relaxed, so the resume waited for atelet's background detach (`--snapshot-detach-delay`, 30 minutes by default). Then it degraded exactly like a durable snapshot.

This behavior is superseded. Since `49ebabe0`, a resume whose node cannot take a paused actor detaches the actor itself, as long as some worker has room, and restores it there. In this setup it would have been restored without memory right away instead of being refused.

## 7. Findings

- **F1, the compatibility keys cannot see worker-binary changes.** Without support, the change broke MEMORY snapshots in both directions and nothing noticed: EIO on every system-info read after an upgrade (E1a), and an actor silently running with another actor's identity after a rollback (E1b). Both actors reported `RUNNING`.
- **F2, a worker-reported attribute works with no protocol change.** `system_info_delivery` flowed through atelet and ate-api-server builds that know nothing about it, because neither validates attribute keys. Matching needed no change: CopyFile snapshots only ever landed on CopyFile-capable workers (5/5 resumes), unstamped snapshots roamed freely (6 resumes across both pools, memory kept every time), and with no capable worker a durable snapshot degraded to ROOTFS with the right identity and a log line.
- **F3, a compatibility fix in the control plane must ship a release before the key.** The version-skew policy upgrades workers before ate-api-server. In that window the old ate-api-server stamped a virtio-fs guest `copyfile` (negative control), and the rollback then cost that actor its memory (`e2neg`). With guest-shape inheritance in place first, the same kind of actor kept its memory across the rollback (`e2old`).
- **F4, the data plane must describe itself.** The `system-info-delivery` marker let each restore pick the guest's real mode (legacy bind or copyfile push) whatever the control plane thought. It is also why the wrong stamp in F3 was only over-restrictive and never unsafe.
- **F5, paused snapshots were pinned, behind a misleading error.** A node-local snapshot that only an upgraded worker can restore was refused with "no worker has room", even on a node with idle capacity, until atelet detached it (30 minutes by default). Superseded by `49ebabe0`: a resume now detaches the actor itself and moves it.
- **F6, CopyFile delivery costs two guarantees.** A file descriptor held across a restore keeps the old contents, so the identity e2e's held-fd assertion changes for copyfile guests. And system-info contents are captured in MEMORY snapshots and published tags. Both are documented in `docs/api-guide.md`. Neither was exercised on the cluster: no MEMORY clone of a CopyFile snapshot was resumed.
- **F7, stored control-plane data has the same problem as snapshots.** A template stored by an earlier deployment could not be decoded by release N's ate-api-server, and that one row stopped golden-snapshot creation for every template in the cluster. Before 1.0 that's allowed (minor upgrades mean reinstall), but it shows the gap is not specific to snapshots.
- **F8, gaps left open by design.**
  - Retiring virtio-fs support would need a default-for-absence rule or a backfill of stored stamps.
  - Golden snapshots built before an upgrade never drain.
  - The key over-stamps actors that have no system-info volumes.

## 8. Limits of this evidence

- **Single runs.** Each step ran once, with one worker per pool, so "5 of 5 on the new pool" was forced by the setup rather than measured. The fallback was triggered by scaling a pool to zero, not by real capacity pressure.
- **Golden snapshots** were disabled (F7). Shared snapshots came from explicit tags instead.
- **The skew policy was broken in places.** The spike ran a new worker beside an old atelet, which the policy forbids.
- **The release split was argued, not executed.** The control-plane and worker releases were rolled in one pass, and the rollback went back two releases at once.
- **The review fixes** (from `29ac10c7` on) were unit-tested but not run on the cluster.

## 9. Not done

- **E2e, live trust-bundle refresh on the cluster.** It needs a CA added to the cluster-wide `egress-mitm-ca-pool`, and the identity e2e suite that would do it needs golden snapshots, which F7 blocks. The syncer is covered by unit tests only.
- **The identity e2e suite on micro-VM against N+1b.** Blocked by F7 for the same reason.
