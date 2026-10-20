# Snapshot compatibility: does the current API support progressive rollouts?

*2026-10-20T19:25:53Z by Showboat 0.6.1*
<!-- showboat-id: 2ce29d33-ade7-44af-8da6-a339e3e3960f -->

This evaluation answers one question: can Substrate's snapshot compatibility API roll out breaking data-plane changes progressively, and if not, what should change? It rests on the draft branch `microvm-system-info-copyfile`, a real experiment on GKE, and an adversarial review of an earlier version of this document, whose corrections are folded in.

Code blocks re-run with `showboat verify` at the branch head (see the README for the command). Tests are named with the commands that run them rather than run here. Cluster results are quoted as captured.

## Verdict

| Kind of breaking change | Today's API | Needed |
|---|---|---|
| Confined to MEMORY state, and the new worker still reads the old format (the branch's change) | **Works**, shown on a real cluster | Code and documentation, no proto field. The control-plane half ships one release before the worker that reports the new key. |
| Changes a snapshot's **disk** format (rootfs or volumes), or drops the old format immediately (for example the micro-VM EROFS change) | **Doesn't work** | A documented matching-rule change, a backfill, and scheduler logic (section 6). Still no proto field. |
| Changes what an existing attribute means | Every memory snapshot loses its memory at once | Only gradual rollout needs more: several compatibility versions per runtime (an existing proto TODO) |

Three qualifications apply throughout:

- **"No proto change" is not "no API change".** The branch changes the documented meaning of two public fields: `Snapshot.sandbox_runtime` and `WorkerAssignment.sandbox_runtime` can now hold a runtime no worker advertises. It also adds a public attribute key and value (`system_info_delivery=copyfile`).
- **Before 1.0 none of this is the supported path.** `docs/upgrade.md` says minor upgrades before 1.0 mean reinstall, the version-skew policy applies from 1.0, and `AGENTS.md` forbids compatibility fallbacks. This is design for 1.0.
- **The evidence is one change, run once.** Section 5 lists what the cluster run did and did not show.

## 1. The API

A snapshot is stamped with the runtime it was captured under:

```bash
sed -n '/^message Snapshot {/,/^}/p' pkg/proto/ateapipb/ateapi.proto | grep -B4 'sandbox_runtime = 8'
```

```output
  // sandbox_runtime is the runtime the actor ran under when the snapshot was
  // taken.
  //
  // +k8s:optional
  SandboxRuntime sandbox_runtime = 8;
```

A runtime is a sandbox class plus a versioned, open-ended set of key/value attributes. The comment on `attributes` is the matching contract (the branch adds the last two sentences about worker-reported and guest-shape attributes):

```bash
sed -n '/^message SandboxRuntime {/,/^}/p;/^message VersionedSandboxCompat {/,/^}/p;/^message AttributeEntry {/,/^}/p' pkg/proto/ateapipb/ateapi.proto
```

```output
message SandboxRuntime {
  // e.g. "gvisor", "microvm"
  //
  // +k8s:required
  // +k8s:maxLength=63
  // +k8s:customValidation # gvisor or microvm
  string sandbox_class = 1;

  // Primary compatibility version stamped onto an actor's snapshot when a
  // checkpoint is taken on this runtime. Snapshots matching `version`
  // can warm-boot on this runtime.
  // TODO: Add support for additional compatibility versions.
  //
  // +k8s:required
  VersionedSandboxCompat version = 2;

  // TODO: Add an informational SandboxConfig version name (e.g. "gvisor-2")
  // once workers report one.
}
message VersionedSandboxCompat {
  // Compatibility schema version (e.g. "v1") defining which runtime and
  // hardware attributes are recorded and how they are computed.
  //
  // +k8s:required
  // +k8s:maxLength=64
  string schema_version = 1;

  // Key-value compatibility attributes defined by this schema version, covering
  // both runtime assets (e.g. "sandbox_bundle_hash") and effective guest
  // hardware features after masking (e.g. "architecture", "cpu_features").
  // Matched against a snapshot under the same schema_version: every attribute
  // on the snapshot must be present on the worker with an equal value (except
  // "cpu_features", where the snapshot's hexadecimal bitmask must be a bitwise
  // subset of the worker's); keys only the worker has are ignored, so a worker
  // can gain keys under one schema_version without invalidating older
  // snapshots. Removing or recomputing a key is a new schema_version, under
  // which older snapshots no longer match.
  // Most attributes are written by atelet; the worker binary reports the ones
  // that describe the guests it boots (e.g. "system_info_delivery"). Those
  // describe a guest's shape, which a guest restored with its memory keeps:
  // when a snapshot lacks one, the runtime its actor is restored under, and
  // so its next snapshot, omits it too.
  // Order as sent does not matter: ate-api-server sorts attributes by key
  // before recording them.
  //
  // +k8s:required
  // +k8s:maxItems=32
  // +k8s:listType=map
  // +k8s:listMapKey=key
  repeated AttributeEntry attributes = 2;
}
message AttributeEntry {
  // Attribute name (e.g. "architecture", "cpu_features", "sandbox_bundle_hash").
  //
  // +k8s:required
  // +k8s:maxLength=128
  string key = 1;

  // Attribute value corresponding to key.
  //
  // +k8s:required
  // +k8s:maxLength=256
  string value = 2;
}
```

The scheduler applies that contract to **MEMORY restores only**. A restore from disk alone is matched on sandbox class and nothing else:

```bash
sed -n '/^func requiredSnapshotRuntime/,/^}/p' cmd/ateapi/internal/controlapi/workflow_resume.go
```

```output
func requiredSnapshotRuntime(actor *ateapipb.Actor, tmpl *ateapipb.ActorTemplate) *ateapipb.SandboxRuntime {
	if localSnap := findLatestLocalSnapshot(actor.GetStatus()); localSnap != nil {
		if localSnap.GetFidelity() != ateapipb.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY {
			return nil
		}
		if templateReplaced(localSnap, tmpl) {
			return nil
		}
		return localSnap.GetSandboxRuntime()
	}
	if snap := findLatestDurableSnapshot(actor.GetStatus(), ateapipb.SnapshotStorageStatus_SNAPSHOT_STORAGE_STATUS_COMPLETED); snap != nil {
		if snap.GetFidelity() != ateapipb.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY {
			return nil
		}
		if templateReplaced(snap, tmpl) {
			return nil
		}
		return snap.GetSandboxRuntime()
	}
	return nil
}
```

The API guide documents the same rule:

```bash
grep -n -A2 'imposes no runtime placement constraint' docs/api-guide.md
```

```output
420:  `SNAPSHOT_FIDELITY_VOLUMES` imposes no runtime placement constraint. Because a
421-  `ROOTFS` restore reuses the container's rootfs upper layer over the template's
422-  image, pools selected by an `ActorTemplate` must share a CPU architecture.
```

## 2. What the branch changes

The branch moves micro-VM system-info volumes (per-actor files atelet writes) from the virtio-fs share to the kata agent's `CopyFile` RPC. That is a breaking change for MEMORY snapshots in both directions. It then makes the change safe to roll out with three pieces:

- **A worker-reported attribute.** The worker binary reports `system_info_delivery=copyfile` on every runtime it registers (`register.ReportConfig.ExtraAttributes`).
- **Guest-shape inheritance.** `hardware.RuntimeFor` drops guest-shape attributes the restored snapshot lacks, so a guest restored with its memory is stamped with the shape it really has.
- **A self-describing snapshot.** Each MEMORY snapshot carries a marker naming its guest's delivery. A missing marker means the snapshot predates the change; an unknown one fails the restore.

Every commit on the branch:

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

Across every proto in the repository, the branch changes comments only:

```bash
git diff --stat 9e921657 HEAD -- '*.proto' | cat; echo; echo 'non-comment proto lines changed:'; git diff 9e921657 HEAD -- '*.proto' | grep -E '^[-+]' | grep -vE '^(\+\+\+|---)' | grep -vE '^[-+][[:space:]]*//' | grep -c . || true
```

```output
 internal/proto/ateletpb/atelet.proto | 2 ++
 pkg/proto/ateapipb/ateapi.proto      | 5 +++++
 2 files changed, 7 insertions(+)

non-comment proto lines changed:
0
```

## 3. Tests on the branch

**Matching and inheritance** (`internal/hardware`). This includes the routing guarantee: a snapshot stamped with the key does not match a worker without it.

    go test -v -run 'TestRuntimeForInheritsGuestShape|TestRuntimeForClonesOnlyWhenDropping|TestCopyFileSnapshotDoesNotMatchLegacyWorker|TestMatchesCompat' ./internal/hardware/

**Worker-reported attributes** (`internal/ateworker/register`):

    go test -v -run 'TestBuildRequestMergesExtraAttributes|TestReconcilerSyncWithExtraAttributesRegistersOnce|TestReportFailsFastOnMissingConfig' ./internal/ateworker/register/

**Scheduling, fallback and stamping** (`cmd/ateapi/internal/controlapi`, PostgreSQL in Docker):

- a key-stamped snapshot lands on a capable worker even when a legacy one has room;
- on a legacy-only fleet it is restored without memory;
- a legacy snapshot restored on a new worker keeps a stamp old workers accept;
- a paused snapshot is never relaxed.

    REQUIRE_DOCKER=true go test -v -run 'TestAssignWorkerAttempt_InheritsGuestShape|TestAssignWorkerAttempt_PrefersRuntimeMatchingWorkerAndFallsBack|TestRequiredSnapshotRuntime|TestCanRelaxRuntime' ./cmd/ateapi/internal/controlapi/

**The data plane** (`cmd/ateworker-microvm`, Linux-only, in a privileged container):

- the snapshot marker, including an unset delivery failing;
- the read-only mount point under the guest tmpfs;
- the advertised key;
- a push cut short by its context failing;
- the syncer's retry, cancellation and handling of projected `.tmp` files.

    docker run --rm --privileged -v "$PWD":/src -w /src golang:1.27 go test -v -run 'TestReadSystemInfoDelivery|TestWriteSystemInfoDelivery|TestSyncsSystemInfo|TestStageSystemInfoCopyDirIsReadOnly|TestWorkerAttributes|TestPushSystemInfo|TestSystemInfoSyncer' ./cmd/ateworker-microvm/

**What no unit test covers.** Mutation testing during review showed that three restore-path decisions have no unit seam:

- whether a MEMORY restore reads the marker;
- whether it stages the legacy share or the CopyFile mount point;
- whether it pushes after resuming.

Stopping the syncer before the checkpoint's pause is not covered either. Each was exercised only by the cluster run in section 5, before the review fixes.

## 4. Why only MEMORY changes work today

The branch's change is the easy kind. Two properties make it work, and a disk-format change has neither:

1. **The change affects only memory**, and memory restores are the ones the scheduler checks. A ROOTFS or VOLUMES restore is placed on any worker of the class (section 1).
2. **The disk part didn't change.** When no worker can restore a snapshot's memory, the fallback restores from disk, which works only because the rootfs and volume formats are the same on both sides.

The micro-VM EROFS change (an open pull request) breaks both. It changes the rootfs upper's format and drops the old one: a new worker rejects an old snapshot that lacks its layer record, and an old worker cannot find the renamed upper tar. A key would not help:

- **New snapshots on old workers.** ROOTFS restores ignore attributes, so new-format snapshots still reach old workers.
- **Old snapshots on new workers.** Old unstamped snapshots still match new workers, because under the matching rule a missing key means no constraint.

## 5. The cluster evidence

GKE `substrate-poc` (two `c3-standard-4` nodes with nested virtualization), release N at `0fdb67b5`, three micro-VM worker builds sharing one SandboxConfig. Full commands, output and image provenance are in `case-study-system-info-copyfile.md`.

| Step | Result |
|---|---|
| Naive build, upgrade | An old snapshot resumed with memory, and every identity read failed with EIO. The actor was `RUNNING` and nothing logged an error. |
| Naive build, rollback | A clone came up with the **source** actor's name and UID. No error. |
| Workers new, ate-api-server still old (negative control) | A legacy guest's next snapshot was stamped `copyfile`, and it lost its memory on rollback for no reason. |
| Upgrade with the key | An old snapshot kept memory and identity on the new build, and kept a stamp old workers accept. |
| Mixed fleet | A key-stamped snapshot landed on the new pool 5 of 5 times; an unstamped one moved between pools 6 times, memory kept. |
| No capable worker | Restored without memory, with its own identity and a logged fallback. |
| Rollback with the key | A clone of a new-format tag restored without memory, with its own identity; the inherited snapshot kept its memory. |

**What the run did not show:**

- **Scale and statistics.** These were single runs, with one worker per pool, so "5 of 5" was forced. The fallback was triggered by scaling a pool to zero, not by real capacity pressure.
- **Golden snapshots.** They were disabled, because the cluster's template reconciler failed on a stored template it could not decode. Shared snapshots came from explicit tags.
- **The new mode's main path.** No MEMORY clone of a CopyFile snapshot was resumed, so the cluster never showed that such a clone gets its own identity.
- **The skew policy and the release split.** The spike ran a new worker beside an old atelet, which the skew policy forbids. The two releases were rolled in one pass, and the rollback went back two releases at once.
- **Today's paused-actor behavior.** The paused-actor result (refused until the 30-minute detach) predates `49ebabe0`. A resume now detaches and moves the actor right away.
- **The review fixes.** The commits from `29ac10c7` on were not run on the cluster.

## 6. Proposal: routing disk-format changes without a new field

The requirement is that a disk-only restore be checked against at least some attributes. These are the options considered:

| Option | API change | Assessment |
|---|---|---|
| Reinstall | none | Today's policy, and what the EROFS change already does. No progressive rollout. |
| Hard-coded list of disk-scoped keys in ate-api-server | none | Works, but each such key needs a control-plane release first. |
| **Naming convention:** keys named `rootfs.*` or `volumes.*` are also checked for disk restores | **No schema change.** The rule goes in the `VersionedSandboxCompat` comment and the API guide. | **Recommended.** It routes like a typed field would, with no extra release per key. |
| A new sandbox class for the new format | No schema change, but large code changes | No fallback between formats, and two pool types through the transition |
| A `scope` field on `AttributeEntry` | Yes, additive | Same routing as the convention; worth it only if typed validation matters |
| Separate stamps per snapshot layer | Yes, larger | More than the problem needs |

With any of them, two more things are required:

- **A backfill.** When such a key is introduced, a one-time migration stamps existing snapshots with the old value (for example `rootfs.layout=host-merged`). Otherwise old unstamped snapshots still match new workers.
- **Scheduler logic.** `requiredSnapshotRuntime` and the fallback have to work per restore type, picking the most complete restore some worker can do, or refusing.

**Guest-shape classification** (the hard-coded list in `internal/hardware`) is not proposed as a public field. Without it, the only cost is lost memory on rollback, never corruption, because the snapshot marker keeps restores correct. If guest-shape keys multiply, the sturdier fix is internal: have the data plane report which attributes a checkpoint depends on, rather than adding a public `kind` field.

## 7. Playbook for a progressive rollout (after 1.0)

1. **Classify the change.**

   | The change alters… | Mechanism |
   |---|---|
   | runtime binaries | a new SandboxConfig version, with the old one kept `Enabled` |
   | memory-only guest shape decided by the worker | a worker-reported guest-shape attribute, as on this branch |
   | a host property | an atelet-reported attribute |
   | a disk format | a `rootfs.*` or `volumes.*` key under the section 6 rule, plus a backfill |
   | an attribute's meaning | a new `schema_version` |

2. **Make the snapshot self-describing.** Write a marker with explicit values for both old and new formats, taken from the running guest rather than from the binary. On restore, a missing marker means the old format and an unknown value fails.
3. **Keep reading the old format** in the new worker, if old snapshots must keep their memory.
4. **Release the control plane first, inert:** the inheritance or matching changes, and any backfill. Roll it in policy order.
5. **Release the worker that reports the key,** pool by pool. Suspend running actors before each pool roll, since the roll doesn't. Then check:
   - the worker's runtimes show the key;
   - new snapshots carry it;
   - legacy guests restored on new workers keep stamps without it.
6. **Gate on correctness, not just capacity.** Watch the fallback warning (`Assigned worker cannot restore the snapshot's runtime`), but know what it doesn't show:
   - **Fallbacks follow capacity.** Actors cold-booted on new pools are pinned to them, so fallbacks rise as those pools fill, whatever the correctness.
   - **Golden-snapshot timing decides adoption.** Goldens built before the upgrade keep their clones in the old shape. Goldens built during it pin clones to new pools.
   - **The key is unconditional.** The worker stamps it on every actor, including actors with no system-info volumes.
7. **Know the cost of a rollback.** State isn't corrupted, but every new-format guest restores without memory. Paused actors are detached and moved on resume.
8. **Retire the old format only after the backfill,** and after counting the snapshots still in the old shape.

## 8. Recommendation

- **Keep the proto schema as it is.** Document the semantic changes to `sandbox_runtime`.
- **If progressive rollout of disk-format changes is wanted for 1.0:** adopt the `rootfs.*` / `volumes.*` rule, the backfill and the scheduler logic from section 6.
- **Keep the branch's pieces for memory-only changes:** worker-reported attributes, guest-shape inheritance, snapshot markers, and the control plane shipping first.
- **Before 1.0:** keep reinstall as the supported path.

