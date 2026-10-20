# Snapshot compatibility and progressive rollouts

These documents examine whether Substrate's snapshot compatibility API (Actor Lifecycle v2: the `SandboxRuntime` stamp on each snapshot and the `AttributeEntry` matching in `internal/hardware`) can roll out breaking data-plane changes progressively, and what to change where it can't.

## Verdict in brief

- **Works today:** a breaking change confined to MEMORY state, where the new worker can still read the old format. The branch's change (micro-VM system-info volumes delivered with kata `CopyFile`) rolled out on a real GKE cluster with no proto field change: one worker-reported attribute, guest-shape inheritance in ate-api-server, and a snapshot marker. That change still alters the documented meaning of `Snapshot.sandbox_runtime` and `WorkerAssignment.sandbox_runtime`, and adds a public attribute key.
- **Doesn't work:** changes to a snapshot's disk format (rootfs or volumes), such as the micro-VM EROFS change. Only MEMORY restores are matched against runtimes, so old snapshots are placed on workers that cannot read them, and the fallback to a disk restore fails too.
- **Proposal:** no new proto field is required. A documented matching rule (attributes named `rootfs.*` or `volumes.*` are also checked for disk restores), a backfill of stored stamps when such a key is introduced, and scheduler logic. Before 1.0 the supported path for such changes remains reinstall, as `AGENTS.md` and `docs/upgrade.md` say.

## The documents

- **[evaluation.md](evaluation.md):** the verdict, its evidence, the proposal and the alternatives considered, and a rollout playbook.
- **[case-study-system-info-copyfile.md](case-study-system-info-copyfile.md):** the GKE experiment it rests on. First a naive build breaks snapshots in both directions, then the compatibility key handles an upgrade, a mixed fleet, a rollback and paused snapshots. It also records which images came from which commits and what the runs did not show.

## Superseded since the runs

- **Paused snapshots now move instead of waiting.** Since `49ebabe0`, resuming a paused actor whose node cannot take it detaches the actor and restores it elsewhere. Before, it was refused with `ResourceExhausted` until atelet's background detach, which is what the case study recorded on `0fdb67b5`.
- **The review fixes were not run on the cluster.** The branch's last commits (from `29ac10c7` on) harden error paths and the syncer. They were reviewed and unit-tested only.

## Re-running the evidence

Both documents are [Showboat](https://github.com/simonw/showboat) files. Their code blocks quote the branch's code and re-run with `showboat verify` from the repository root:

```bash
uvx showboat verify docs/dev/snapshot-compat/evaluation.md
```

The evaluation names the tests that cover the mechanism, with the commands that run them. The micro-VM tests are Linux-only and run in a container, and the control-plane tests need Docker for PostgreSQL. Cluster results are quoted as captured, since those runs cannot be replayed.
