# Agent Substrate: A Linear Code Walkthrough

*2026-09-26T05:48:46Z by Showboat 0.6.1*
<!-- showboat-id: 97da7ffe-6407-448b-86cf-40716322bec4 -->

This walkthrough follows one **actor** (a sandboxed, mostly idle, agent-like workload) through its whole life in the code. It starts with the API vocabulary. Then it covers how Kubernetes supplies warm **workers**, how the control plane records actors and binds them to workers, and how a resume reaches down through the node agent into `runsc`. It follows a user's HTTP request through the router into the sandbox, and finally shows how the actor is checkpointed back to object storage.

The core idea: Kubernetes starts pods slowly, and idle pods waste resources. So Substrate keeps a pool of pre-started worker pods and multiplexes a much larger set of actors onto them. Idle actors are suspended to snapshots, and a snapshot is restored onto any free worker when traffic arrives.

Every code block below is a real command run against the repo, so `showboat verify walkthrough.md` re-checks that the quoted code is still there.

## 1. The map

Each binary lives under `cmd/`:

```bash
ls cmd
```

```output
ate-setup
ateapi
atecontroller
atelet
atenet
ateom-gvisor
ateom-microvm
benchmarking
credential-provider
dataplane
kubectl-ate
podcertcontroller
```

The ones that matter for the actor lifecycle are:

| Binary | Role |
|---|---|
| `ateapi` (ate-api-server) | The control plane. It is a gRPC API backed by PostgreSQL, owns every actor's state machine, and schedules actors onto workers. |
| `atecontroller` | A Kubernetes controller. It turns `WorkerPool` CRDs into Deployments and mirrors worker pods into ateapi as `Worker` records. |
| `atelet` | A DaemonSet node agent. It pulls images, builds OCI bundles, moves snapshots to and from GCS/S3, and drives ateom. |
| `ateom-gvisor` / `ateom-microvm` | The privileged "herder" inside each worker pod. It runs `runsc` (gVisor) or Cloud Hypervisor (Kata micro-VM) to run, checkpoint, and restore the sandbox, and hosts `atunnel` for traffic. |
| `atenet` | The router: Envoy plus an `ext_proc` server. It resumes actors on demand and tunnels requests to the right worker. |
| `podcertcontroller` | Issues the short-lived pod certificates every component uses for mTLS. |
| `kubectl-ate` | The CLI. |

## 2. The vocabulary: the public API

Everything starts from `pkg/proto/ateapipb/ateapi.proto`. Its `Control` service is the client-facing API:

```bash
grep -n '  rpc ' pkg/proto/ateapipb/ateapi.proto | sed -n '1,40p'
```

```output
27:  rpc GetActor(GetActorRequest) returns (Actor) {}
30:  rpc CreateActor(CreateActorRequest) returns (Actor) {}
33:  rpc UpdateActor(UpdateActorRequest) returns (Actor) {}
38:  rpc SuspendActor(SuspendActorRequest) returns (SuspendActorResponse) {}
41:  rpc PauseActor(PauseActorRequest) returns (PauseActorResponse) {}
44:  rpc ResumeActor(ResumeActorRequest) returns (ResumeActorResponse) {}
48:  rpc RevertActor(RevertActorRequest) returns (RevertActorResponse) {}
52:  rpc DeleteActor(DeleteActorRequest) returns (Actor) {}
55:  rpc GetActorEgressPolicy(GetActorEgressPolicyRequest) returns (EgressPolicy) {}
58:  rpc CreateActorEgressPolicy(CreateActorEgressPolicyRequest) returns (EgressPolicy) {}
61:  rpc UpdateActorEgressPolicy(UpdateActorEgressPolicyRequest) returns (EgressPolicy) {}
64:  rpc DeleteActorEgressPolicy(DeleteActorEgressPolicyRequest) returns (EgressPolicy) {}
69:  rpc MintActorJWT(MintActorJWTRequest) returns (MintActorJWTResponse) {}
77:  rpc MintActorCertificate(MintActorCertificateRequest) returns (MintActorCertificateResponse) {}
82:  rpc CreateTag(CreateTagRequest) returns (Tag) {}
85:  rpc GetTag(GetTagRequest) returns (Tag) {}
88:  rpc ListTags(ListTagsRequest) returns (ListTagsResponse) {}
91:  rpc UpdateTag(UpdateTagRequest) returns (Tag) {}
96:  rpc DeleteTag(DeleteTagRequest) returns (Tag) {}
99:  rpc ListWorkers(ListWorkersRequest) returns (ListWorkersResponse) {}
102:  rpc GetWorker(GetWorkerRequest) returns (Worker) {}
105:  rpc CreateWorker(CreateWorkerRequest) returns (Worker) {}
108:  rpc UpdateWorker(UpdateWorkerRequest) returns (Worker) {}
112:  rpc DeleteWorker(DeleteWorkerRequest) returns (Worker) {}
117:  rpc DrainWorker(DrainWorkerRequest) returns (Worker) {}
120:  rpc ListWorkerActorAssignments(ListWorkerActorAssignmentsRequest) returns (ListWorkerActorAssignmentsResponse) {}
123:  rpc ListActors(ListActorsRequest) returns (ListActorsResponse) {}
126:  rpc CreateAtespace(CreateAtespaceRequest) returns (Atespace) {}
129:  rpc GetAtespace(GetAtespaceRequest) returns (Atespace) {}
132:  rpc ListAtespaces(ListAtespacesRequest) returns (ListAtespacesResponse) {}
136:  rpc DeleteAtespace(DeleteAtespaceRequest) returns (Atespace) {}
138:  rpc CreateActorTemplate(CreateActorTemplateRequest) returns (ActorTemplate) {}
140:  rpc GetActorTemplate(GetActorTemplateRequest) returns (ActorTemplate) {}
142:  rpc ListActorTemplates(ListActorTemplatesRequest) returns (ListActorTemplatesResponse) {}
146:  rpc DeleteActorTemplate(DeleteActorTemplateRequest) returns (ActorTemplate) {}
2045:  rpc SetWorkerCapacity(SetWorkerCapacityRequest) returns (SetWorkerCapacityResponse);
2051:  rpc MintAteomActorCertificate(MintAteomActorCertificateRequest) returns (MintAteomActorCertificateResponse);
2072:  rpc RequestActorSuspend(RequestActorSuspendRequest) returns (RequestActorSuspendResponse);
```

There are five resource kinds. An **Atespace** is the isolation boundary, and an actor is addressed as `atespace/name`. An **ActorTemplate** is the immutable "class" (images, resources, snapshot policy). An **Actor** is one instance. A **Tag** is a named, retained copy of a snapshot. A **Worker** is one warm pod. None of these are Kubernetes objects: they live in Postgres because they change far too often for etcd.

The last three RPCs above belong to a second service, `WorkerService`. Only atelet calls it, to report what a worker observes.

The heart of the model is the actor state machine:

```bash
sed -n '529,541p' pkg/proto/ateapipb/ateapi.proto
```

```output
enum ActorState {
  ACTOR_STATE_UNSPECIFIED = 0;
  ACTOR_STATE_RESUMING = 1;
  ACTOR_STATE_RUNNING = 2;
  ACTOR_STATE_SUSPENDING = 3;
  ACTOR_STATE_SUSPENDED = 4;
  ACTOR_STATE_PAUSING = 5;
  ACTOR_STATE_PAUSED = 6;
  ACTOR_STATE_CRASHED = 7;
  ACTOR_STATE_DELETING = 8;
  ACTOR_STATE_REVERTING = 9;
  // Keep this in sync with ActorStatus.state's maximum.
}
```

Every `-ING` state is a transient marker that a multi-step workflow writes before it touches the outside world. That is what makes the workflows restartable, as section 6 shows. The actor's status also records where it is running and which snapshot it resumes from:

```bash
sed -n '543,575p' pkg/proto/ateapipb/ateapi.proto
```

```output
message ActorStatus {
  // state is the Actor's current lifecycle state.
  //
  // +k8s:required
  // +k8s:minimum=1
  // +k8s:maximum=9 # keep this in sync with the ActorState enum
  ActorState state = 1;

  // worker_assignment points at the worker currently hosting this Actor.
  // Unset whenever the Actor has no worker (SUSPENDED, PAUSED, CRASHED).
  //
  // +k8s:optional
  // +k8s:update=NoModify # can be set and cleared, but not changed in place
  WorkerAssignment worker_assignment = 2;

  // in_progress_snapshot_uri is the URI in object storage of the durable
  // snapshot the Actor is currently taking.
  //
  // +k8s:optional
  // +k8s:maxLength=2048 # the template's storage_location bound plus the owner prefix and snapshot name
  string in_progress_snapshot_uri = 3;

  // external_snapshot is the Actor's current external snapshot.
  // If the Actor was created from a Tag this is the tag's snapshot, borrowed
  // until the Actor's first suspend writes one of its own. Otherwise it is
  // unset until the Actor is first suspended.
  // +k8s:optional
  ExternalSnapshot external_snapshot = 4;

  // Node-local state used only while the Actor is paused.
  //
  // +k8s:optional
  LocalSnapshot local_snapshot = 5;
```

The `+k8s:required` and `+k8s:maxLength` comments are not decoration. `hack/update/codegen.sh` runs the Kubernetes `validation-gen` tool over these protos and emits `cmd/ateapi/internal/controlapi/zz_generated.validation.go`, about 8k lines of `Validate_<Type>Request` functions that every handler calls first.

### Three tiers of RPC

Beneath `Control` are two internal services, defined in `internal/proto/`. ateapi calls **atelet's** `AteomHerder` service over the network, and atelet calls **ateom's** `Ateom` service over a unix socket inside the node:

```bash
grep -n 'service \|  rpc ' internal/proto/ateletpb/atelet.proto internal/proto/ateompb/ateom.proto
```

```output
internal/proto/ateletpb/atelet.proto:27:service AteomSupport {
internal/proto/ateletpb/atelet.proto:33:  rpc MintActorCertificate(MintActorCertificateRequest) returns (MintActorCertificateResponse) {}
internal/proto/ateletpb/atelet.proto:36:  rpc SetWorkerCapacity(SetWorkerCapacityRequest) returns (SetWorkerCapacityResponse) {}
internal/proto/ateletpb/atelet.proto:47:  rpc RequestActorSuspend(RequestActorSuspendRequest) returns (RequestActorSuspendResponse) {}
internal/proto/ateletpb/atelet.proto:97:service AteomHerder {
internal/proto/ateletpb/atelet.proto:100:  rpc Run(RunRequest) returns (RunResponse) {}
internal/proto/ateletpb/atelet.proto:105:  rpc Checkpoint(CheckpointRequest) returns (CheckpointResponse) {}
internal/proto/ateletpb/atelet.proto:108:  rpc Restore(RestoreRequest) returns (RestoreResponse) {}
internal/proto/ateletpb/atelet.proto:114:  rpc UploadPausedCheckpoint(UploadPausedCheckpointRequest) returns (UploadPausedCheckpointResponse) {}
internal/proto/ateletpb/atelet.proto:118:  rpc Terminate(TerminateRequest) returns (TerminateResponse) {}
internal/proto/ateompb/ateom.proto:34:service Ateom {
internal/proto/ateompb/ateom.proto:37:  rpc RunWorkload(RunWorkloadRequest) returns (RunWorkloadResponse) {}
internal/proto/ateompb/ateom.proto:42:  rpc CheckpointWorkload(CheckpointWorkloadRequest) returns (CheckpointWorkloadResponse) {}
internal/proto/ateompb/ateom.proto:47:  rpc RestoreWorkload(RestoreWorkloadRequest) returns (RestoreWorkloadResponse) {}
internal/proto/ateompb/ateom.proto:79:  rpc GetWorkloadStats(GetWorkloadStatsRequest) returns (GetWorkloadStatsResponse) {}
internal/proto/ateompb/ateom.proto:102:  rpc GetActiveWorkloadStats(GetActiveWorkloadStatsRequest) returns (GetActiveWorkloadStatsResponse) {}
internal/proto/ateompb/ateom.proto:106:  rpc TerminateWorkload(TerminateWorkloadRequest) returns (TerminateWorkloadResponse) {}
```

So a resume is `ateapi.ResumeActor` → `atelet.Restore` → `ateom.RestoreWorkload` → `runsc restore`. The reverse channel, `AteomSupport`, lets the sandbox herder ask atelet for an atunnel certificate, report its capacity, or propose that its own actor be suspended.

## 3. Warm capacity: from WorkerPool to Worker records

A substrate admin declares warm capacity with a `WorkerPool` CRD (`pkg/api/v1alpha1/workerpool_types.go`). With the doc comments stripped, the spec is small:

```bash
sed -n '80,108p' pkg/api/v1alpha1/workerpool_types.go | grep -v '^\s*//'
```

```output
type WorkerPoolSpec struct {
	Replicas int32 `json:"replicas"`

	WorkerImage string `json:"workerImage"`

	Template *WorkerPoolPodTemplate `json:"template,omitempty"`

	SandboxClass SandboxClass `json:"sandboxClass,omitempty"`
}
```

`atecontroller` reconciles each pool into a server-side-applied `Deployment` (`cmd/atecontroller/internal/controllers/workerpool_apply.go`). The worker pod runs a single container, the `ateom` image for the pool's sandbox class. Its arguments show the atunnel listeners that later carry actor traffic: ingress on 443, HTTP CONNECT on 8443, and egress on 15001:

```bash
sed -n '108,122p;139,162p' cmd/atecontroller/internal/controllers/workerpool_apply.go
```

```output

	args := []string{
		"--pod-uid=$(POD_UID)",
		"--atunnel-listen-address=:443",
		"--atunnel-connect-listen-address=:8443",
		"--atunnel-credential-bundle=" + atunnelIdentityMountPath + "/credential-bundle.pem",
		"--atunnel-trust-bundle=" + atunnelIdentityMountPath + "/trust-bundle.pem",
		// The peers atunnel authenticates live in substrate's namespace, not
		// the worker's, so the controller passes their identities rather than
		// letting ateom assume the default install. --atunnel-client-identity
		// has been accepted by every ateom that carries this controller's
		// contemporaries, so it is always safe to pass.
		"--atunnel-client-identity=" + installdefaults.SPIFFEID(systemNamespace, routerServiceAccount),
	}

	)

	containerAC := corev1ac.Container().
		WithName("ateom").
		WithImage(wp.Spec.WorkerImage).
		WithArgs(args...).
		WithPorts(corev1ac.ContainerPort().
			WithName("https").
			WithContainerPort(443).
			WithProtocol(corev1.ProtocolTCP),
			corev1ac.ContainerPort().
				WithName("connect").
				WithContainerPort(8443).
				WithProtocol(corev1.ProtocolTCP),
			corev1ac.ContainerPort().
				WithName("readyz").
				WithContainerPort(8080).
				WithProtocol(corev1.ProtocolTCP)).
		WithReadinessProbe(corev1ac.Probe().
			WithHTTPGet(corev1ac.HTTPGetAction().
				WithPath("/readyz").
				WithPort(intstr.FromString("readyz")))).
		WithSecurityContext(ateomSecurityContext(wp.Spec.SandboxClass)).
		WithEnv(ateomContainerEnv(otel)...).
```

Kubernetes pods are not visible to the scheduler until they are copied into ateapi as `Worker` records. That is the job of the `WorkerPoolSyncer` (`cmd/atecontroller/internal/workersync/syncer.go`). It watches worker pods with an informer and maps each pod transition to one ateapi call. A deleted pod becomes `DeleteWorker`, a Terminating pod becomes `DrainWorker`, and a Ready pod with an IP becomes `CreateWorker`:

```bash
sed -n '244,277p' cmd/atecontroller/internal/workersync/syncer.go
```

```output
func (s *WorkerPoolSyncer) reconcile(ctx context.Context, key workerKey) error {
	obj, exists, err := s.workerInformer.GetIndexer().GetByKey(key.namespace + "/" + key.name)
	if err != nil {
		return err
	}
	if !exists {
		slog.InfoContext(ctx, "Syncer: deregistering worker (pod deleted)", key.logAttrs()...)
		return s.reconcileDeadWorker(ctx, key)
	}
	pod := obj.(*corev1.Pod)
	if string(pod.UID) != key.uid {
		// The pod was deleted and a new one took its name. This key names the
		// dead incarnation; the live pod was enqueued under its own key.
		slog.InfoContext(ctx, "Syncer: deregistering worker (pod replaced)", key.logAttrs()...)
		return s.reconcileDeadWorker(ctx, key)
	}
	// Checked before eligibility: draining works off the registered record by name
	// and never reads the pod IP, while a Terminating pod can legitimately report
	// no IP once its sandbox is torn down. Gating on the IP first would drop the
	// transition and leave the worker schedulable for as long as the pod lingers.
	if pod.DeletionTimestamp != nil {
		// The pod has entered Terminating: mark the worker DRAINING so the
		// scheduler stops routing new actors to it. We deliberately do NOT touch
		// the bound actor here — inside the pod ateom has received SIGTERM and is
		// gracefully shutting the actor down. Actor cleanup happens on the Pod
		// Deleted event.
		return s.markWorkerDraining(ctx, key)
	}
	if !isWorkerEligible(pod) {
		// The pod has no IP or is not Ready yet; a later update event re-enqueues it.
		return nil
	}
	return s.createOrUpdateWorker(ctx, key, pod)
}
```

The Worker record carries what the scheduler needs: the pod IP, node name, sandbox class, and pool labels. Capacity is deliberately left out, because only the ateom inside the pod knows what it can supply. It reports that later through `atelet` → `WorkerService.SetWorkerCapacity`. Until then a new Worker can hold only one actor.

```bash
sed -n '293,318p' cmd/atecontroller/internal/workersync/syncer.go
```

```output
	w, err := s.client.GetWorker(ctx, &ateapipb.GetWorkerRequest{Worker: key.workerRef()})
	if status.Code(err) == codes.NotFound {
		slog.InfoContext(ctx, "Syncer: registering worker", key.logAttrs()...)
		worker := &ateapipb.Worker{
			// Workers are global-scoped, so the name carries no atespace. See
			// workerKey.workerName for where the name comes from.
			Metadata:        &ateapipb.ResourceMetadata{Name: key.workerName()},
			WorkerNamespace: pod.Namespace,
			WorkerPool:      poolName,
			WorkerPod:       pod.Name,
			Ip:              pod.Status.PodIP,
			WorkerPodUid:    string(pod.UID),
			NodeName:        pod.Spec.NodeName,
			SandboxClass:    string(pool.Spec.SandboxClass),
			Labels:          pool.GetLabels(),
			// Capacity is the Worker's to report, not the syncer's to infer
			// from the pod: it is what the ateom can actually supply. Until
			// that report lands, CreateWorker's reified ceiling holds the
			// Worker to a single Actor.
		}
		// status is output-only: CreateWorker sets STATE_ACTIVE itself.
		//
		// ALREADY_EXISTS means we lost a create race; requeue and converge via
		// the update path. INVALID_ARGUMENT is terminal — see
		// processNextWorkItem.
		_, err := s.client.CreateWorker(ctx, &ateapipb.CreateWorkerRequest{Worker: worker})
```

## 4. The control plane boots

`cmd/ateapi/main.go` connects to Postgres, builds an embedded OpenFGA authorization server on the same connection pool, starts informers for WorkerPool and SandboxConfig, and wires everything into `controlapi.NewRPCService`. It then starts the golden-snapshot reconciler (section 5) and a gRPC server with two services. Authentication is the first interceptor in the chain:

```bash
sed -n '275,311p' cmd/ateapi/main.go
```

```output
	// Drive stored ActorTemplates through the golden actor flow.
	templateReconciler := controlapi.NewActorTemplateReconciler(persistence, controlSrv, *templateResyncInterval)
	templateReconciler.Start(shutdownCtx)

	lisCfg := &net.ListenConfig{}
	lis, err := lisCfg.Listen(ctx, "tcp", *listenAddr)
	if err != nil {
		serverboot.Fatal(ctx, "Failed to start listener", err)
	}

	if err := apiauthn.ValidateServerConfig(authCfg); err != nil {
		serverboot.Fatal(ctx, "Invalid auth config", err)
	}

	mux := grpc.NewServer(
		grpc.Creds(serverCreds),
		grpc.StatsHandler(otelgrpc.NewServerHandler()),
		// Close connections after an hour to allow for any
		// client that doesn't use Kubernetes endpoint resolvers
		// to eventually reobtain backend IPs. https://github.com/grpc/grpc/issues/12295
		grpc.KeepaliveParams(keepalive.ServerParameters{
			MaxConnectionAge:      1 * time.Hour,
			MaxConnectionAgeGrace: maxRPCDeadline + time.Minute,
		}),
		grpc.ChainUnaryInterceptor(
			apiauthn.UnaryServerInterceptor(authCfg),
			ateinterceptors.MaxDeadlineUnaryInterceptor(maxRPCDeadline),
			ateinterceptors.ServerUnaryInterceptor,
			ateinterceptors.RejectUnknownFieldsUnaryInterceptor,
		),
		grpc.ChainStreamInterceptor(
			apiauthn.StreamServerInterceptor(authCfg),
		),
	)
	reflection.Register(mux)
	ateapipb.RegisterControlServer(mux, controlSrv)
	ateapipb.RegisterWorkerServiceServer(mux, workerservice.New(persistence, controlSrv, ateletSPIFFEID, actorIDCAPool))
```

`apiauthn` (`cmd/ateapi/internal/apiauthn/apiauthn.go`) accepts either an mTLS peer, identified by the SPIFFE URI SAN in its certificate, or a bearer JWT. The JWT is verified against the issuer named in its `iss` claim, with keys found through OIDC discovery (`internal/oidcjwt`). Per-RPC authorization is not enforced yet: the code has `TODO(authz)` markers and only authentication runs.

### The store: proto-shaped rows with optimistic concurrency

`cmd/ateapi/internal/store/store.go` defines `store.Interface`. It has no Go model structs of its own: it stores the API protos directly. Every mutation is a read-modify-write guarded by a `Precondition` of uid plus version, so a blind write is rejected:

```bash
sed -n '81,102p' cmd/ateapi/internal/store/store.go
```

```output

	// UpdateActor performs a transactional read-modify-write and returns the stored
	// actor with advanced metadata (version, update_time).
	//
	// precondition guards the write against landing on unexpected state: it is
	// checked against the stored actor before mutate runs. Both the uid and
	// version guards are required.
	//
	// mutate receives the stored actor and edits it in place. The mutated actor is
	// written iff mutate returns nil.
	//
	// mutate may run more than once, because the store retries when a concurrent
	// write invalidates the transaction.
	//
	// Returns ErrPreconditionRequired if the precondition omits either guard,
	// ErrNotFound if missing, ErrUIDConflict or ErrVersionConflict if the
	// precondition no longer holds, ErrVersionConflict if the retry budget is
	// exhausted, or the mutate's error verbatim otherwise. Immutable fields
	// are not checked here; the service layer enforces them via declarative
	// validation before the write.
	UpdateActor(ctx context.Context, actorRef resources.ActorRef, precondition Precondition, mutate func(toUpdate *ateapipb.Actor) error) (*ateapipb.Actor, error)

```

Binding an actor to a worker is the one operation that must never double-book. The Postgres implementation (`cmd/ateapi/internal/store/atepg/worker_assignment.go`) does it in a single transaction. It locks the worker row with `SELECT ... FOR UPDATE`, inserts the assignment and lets `ON CONFLICT` report whether the actor was already bound, then runs the caller's `admit` capacity check *under the lock*:

```bash
sed -n '87,122p' cmd/ateapi/internal/store/atepg/worker_assignment.go
```

```output
	_, err = p.writeAndAppendEvent(ctx, store.WorkerEventUpdated, func(ctx context.Context, tx pgx.Tx) (*ateapipb.Worker, error) {
		worker, err := getWorkerForUpdate(ctx, tx, workerName)
		if err != nil {
			return nil, err
		}
		if worker.Status == nil {
			worker.Status = &ateapipb.WorkerStatus{}
		}

		// Insert first and let the conflict say whether the Actor was already
		// bound. Checking with a read instead would miss a claim that commits
		// after it, and both claims would believe they were first.
		tag, err := tx.Exec(ctx, `
			INSERT INTO worker_assignments (actor_uid, worker_name, proto)
			VALUES ($1, $2, $3)
			ON CONFLICT (actor_uid) DO NOTHING`,
			actorUID, workerName, assignmentBytes)
		if err != nil {
			return nil, fmt.Errorf("binding actor %s to worker %s: %w", actorUID, workerName, err)
		}
		if tag.RowsAffected() == 1 {
			// A new binding needs room. The row lock holds the answer until
			// commit, and a refusal rolls the insert back.
			if admit != nil {
				if err := admit(worker); err != nil {
					return nil, err
				}
			}
			allocated, err := resources.AddToAllocated(worker.Status.Allocated, assignment, +1)
			if err != nil {
				return nil, err
			}
			worker.Status.Allocated = allocated
			if err := saveWorker(ctx, tx, worker); err != nil {
				return nil, err
			}
```

`writeAndAppendEvent` wraps that transaction in a **transactional outbox**. Every worker mutation also inserts an event row into `worker_outbox` in the same transaction, so an event exists if and only if the write committed. The file's header summarizes the design:

```bash
sed -n '15,20p' cmd/ateapi/internal/store/atepg/outbox.go
```

```output
// The worker outbox: worker writes append one event row to the
// range-partitioned, UNLOGGED worker_outbox table in the same transaction
// (writeAndAppendEvent), per-replica watchers poll it with an xmin-fenced
// xid cursor (WatchWorkers), and a background loop pre-creates partitions
// and retires old ones by dropping them, recording a trim high-water mark
// that lets lagging watchers detect loss and resync (outboxMaintenance).
```

```bash
sed -n '79,110p' cmd/ateapi/internal/store/atepg/outbox.go
```

```output
func (p *Persistence) writeAndAppendEvent(ctx context.Context, eventType store.WorkerEventType, fn func(ctx context.Context, tx pgx.Tx) (*ateapipb.Worker, error)) (*ateapipb.Worker, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op once committed

	worker, err := fn(ctx, tx)
	if err != nil {
		return nil, err
	}

	var payload []byte
	if worker != nil {
		payload, err = marshalWorkerEvent(eventType, worker)
		if err != nil {
			return nil, fmt.Errorf("marshaling worker event: %w", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO worker_outbox (payload) VALUES ($1)`, payload); err != nil {
			return nil, fmt.Errorf("appending worker outbox: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("committing transaction: %w", err)
	}
	if payload != nil {
		p.publishLocally(ctx, payload)
	}
	return worker, nil
}

```

Why bother? Each ateapi replica keeps an in-memory `workercache`, fed by `WatchWorkers` polling that outbox, and the **scheduler reads only that cache**, never Postgres. That keeps scheduling off the database's hot path. A stale cache entry is harmless, because the `admit` callback re-checks capacity under the row lock. The scheduler itself (`cmd/ateapi/internal/scheduling/scheduling.go`) is deliberately simple: filter, then pick at random.

```bash
sed -n '99,118p' cmd/ateapi/internal/scheduling/scheduling.go
```

```output
// Schedule filters the current worker fleet to find unassigned candidates matching the given constraints.
func (s *scheduler) Schedule(ctx context.Context, constraints Constraints) (*ateapipb.Worker, error) {
	workers, err := s.source.Workers()
	if err != nil {
		return nil, fmt.Errorf("while listing workers: %w", err)
	}

	var candidates []*ateapipb.Worker
	for _, worker := range workers {
		if s.Applies(worker, constraints) && s.HasRoom(worker, constraints) {
			candidates = append(candidates, worker)
		}
	}

	if len(candidates) == 0 {
		return nil, ErrNoCapacity
	}

	return candidates[s.intn(len(candidates))], nil
}
```

## 5. Creating things: templates, golden snapshots, actors

### CreateActor is just a database row

Creating an actor does not touch a worker at all. `ServiceImpl.CreateActor` (`cmd/ateapi/internal/controlapi/actor.go`) resolves the template, then picks a source tag: an explicit one, or the template's **golden tag**. It records the actor as `SUSPENDED` and *borrows* the tag's snapshot. A fresh actor is therefore "a suspended copy of the golden snapshot", and its first resume is just a restore:

```bash
sed -n '91,95p;124,144p' cmd/ateapi/internal/controlapi/actor.go
```

```output
	// Resolve the explicit tag, or freeze the template's current golden default.
	tagRef := inActor.GetSourceTag()
	if tagRef == nil {
		tagRef = template.GetStatus().GetGoldenSnapshotStatus().GetGoldenTag()
	} else {

	// Verify that the result is properly valid before storing it.
	outActor := proto.CloneOf(inActor)
	outActor.Status = &ateapipb.ActorStatus{
		State:        ateapipb.ActorState_ACTOR_STATE_SUSPENDED,
		ActorVolumes: initVols,
	}
	if sourceTag != nil {
		// The Actor starts out borrowing the tag's external snapshot rather than
		// copying it. The snapshot URI is under the tag's prefix, not the Actor's, which
		// is what keeps the Actor from collecting those objects. Its first
		// suspend writes a snapshot under its own prefix and takes over from
		// there.
		outActor.Status.ExternalSnapshot = proto.CloneOf(sourceTag.GetStatus().GetSnapshot())
		// The Actor is born with guest state, so stamp the template that state
		// was built on now rather than at the first resume. The Tag records it
		// beside its snapshot rather than on it, so the clone above does not
		// carry it. Left empty, a repoint before that first resume reads as "no
		// guest state" instead of "replaced template", and the resume restores
		// the old template's memory and rootfs in full instead of the volume
		// data alone.
```

### Where golden snapshots come from

`CreateActorTemplate` (`actor_template.go`) only validates the template and stores it. The interesting part is the background `ActorTemplateReconciler` (`template_reconciler.go`). For each template it runs a small state machine over a hidden **golden actor**, which is named after the template's UID and lives in the reserved `ate-golden` atespace. The reconciler creates that actor, resumes it (with no snapshot, so it cold-boots), waits out a warmup deadline, suspends it, and copies the resulting snapshot into a Tag. That Tag becomes `status.goldenSnapshotStatus.goldenTag`, which is what `CreateActor` borrowed above.

Notice that the reconciler drives the golden actor through the same public `ResumeActor`/`SuspendActor` workflows as any user actor. Each loop iteration observes the actor's state and takes the one action that state calls for:

```bash
sed -n '248,300p' cmd/ateapi/internal/controlapi/template_reconciler.go
```

```output

		case ateapipb.ActorState_ACTOR_STATE_RUNNING:
			takeAt := goldenSnapshotStatus.GetTakeGoldenSnapshotAt()
			if takeAt == nil {
				// Resumed, but the deadline write was lost (a replica died
				// after ResumeActor). The elapsed warmup is unknowable, so
				// restart the clock.
				slog.WarnContext(ctx, "Golden actor running without a snapshot deadline; restarting warmup", slog.String("ActorTemplate", ref.String()))
				deadline := time.Now().Add(goldenSnapshotWarmupFor(tmpl.GetContainers()))
				if tmpl, err = r.checkpoint(ctx, tmpl, func(snapshotStatus *ateapipb.GoldenSnapshotStatus) {
					snapshotStatus.TakeGoldenSnapshotAt = timestamppb.New(deadline)
				}); err != nil {
					return 0, err
				}
				continue
			}
			// Not time to take the golden snapshot yet; requeue.
			if rem := time.Until(takeAt.AsTime()); rem > 0 {
				return rem, nil
			}
			// Warmup done: suspend the golden actor and record its snapshot.
			err := r.suspendActor(ctx, goldenActorRef)
			if err != nil {
				return 0, err
			}
			return 0, r.tagGoldenActor(ctx, tmpl, goldenActorRef)

		case ateapipb.ActorState_ACTOR_STATE_SUSPENDING:
			// A previous pass died mid-suspend; retry suspend.
			err := r.suspendActor(ctx, goldenActorRef)
			if err != nil {
				return 0, err
			}
			return 0, r.tagGoldenActor(ctx, tmpl, goldenActorRef)

		case ateapipb.ActorState_ACTOR_STATE_RESUMING,
			ateapipb.ActorState_ACTOR_STATE_SUSPENDED:
			// The golden actor was never resumed, or a previous resume didn't
			// finish; ResumeActor is reentrant from both.
			if actor.GetStatus().GetExternalSnapshot().GetSnapshotUri() != "" {
				// Golden actors never start from a source snapshot, so an
				// existing snapshot means an earlier suspend completed
				// without being recorded.
				return 0, r.tagGoldenActor(ctx, tmpl, goldenActorRef)
			}
			if _, err := r.control.ResumeActor(ctx, &ateapipb.ResumeActorRequest{Actor: goldenActorRef}); err != nil {
				// A crash during resume is observed as CRASHED on the retry.
				return 0, fmt.Errorf("while resuming golden actor: %w", err)
			}
			deadline := time.Now().Add(goldenSnapshotWarmupFor(tmpl.GetContainers()))
			if tmpl, err = r.checkpoint(ctx, tmpl, func(snapshotStatus *ateapipb.GoldenSnapshotStatus) {
				snapshotStatus.TakeGoldenSnapshotAt = timestamppb.New(deadline)
			}); err != nil {
```

## 6. ResumeActor: the heart of the control plane

Every lifecycle operation in `cmd/ateapi/internal/controlapi/` is an `ActorWorkflow` method built from the same pattern:

1. Take a **per-actor distributed lease** (a Postgres row). A concurrent operation on the same actor fails fast with `Aborted`.
2. Run a list of `ensureX` steps. Each step is **idempotent** and works out its own progress from the persisted actor. If a replica dies halfway, the next caller re-enters the workflow and fast-forwards past the steps already done.
3. Commit each state transition with a uid+version precondition.

The lease is a thin wrapper over `store.AcquireLease`:

```bash
sed -n '204,218p' cmd/ateapi/internal/controlapi/workflow.go
```

```output
func acquireLease(ctx context.Context, holder leaseHolder, key, subject string) (context.Context, *store.Lease, error) {
	lease, err := holder.AcquireLease(ctx, key)
	if err != nil {
		if errors.Is(err, store.ErrLeaseConflict) {
			return nil, nil, status.Errorf(grpcCodes.Aborted, "another operation is in progress for this %s", subject)
		}
		return nil, nil, fmt.Errorf("while acquiring lease: %w", err)
	}

	return lease.Context(), lease, nil
}

func (w *ActorWorkflow) acquireActorLease(ctx context.Context, actorRef resources.ActorRef) (context.Context, *store.Lease, error) {
	return acquireLease(ctx, w.store, "lease:actor:"+actorRef.Atespace+":"+actorRef.Name, "actor")
}
```

`ResumeActor` in `workflow_resume.go` reads as a table of contents for the whole resume. Note the **lock-free fast path** first: the router calls `ResumeActor` on *every* routed request, so an actor that is already `RUNNING` must cost just one read, without a lease write:

```bash
sed -n '87,135p' cmd/ateapi/internal/controlapi/workflow_resume.go
```

```output
	// Routed requests call ResumeActor even when the actor is already running.
	// Read before taking the distributed lease so that hot-path checks do not
	// upsert and delete a PostgreSQL lease row. Any state that needs work is read
	// again under the lease below.
	actor, err = w.store.GetActor(ctx, actorRef)
	if err != nil {
		return nil, false, err
	}
	if wasRunning = actor.GetStatus().GetState() == ateapipb.ActorState_ACTOR_STATE_RUNNING; wasRunning {
		return actor, false, nil
	}

	leaseCtx, lease, err := w.acquireActorLease(ctx, actorRef)
	if err != nil {
		return nil, false, err
	}
	defer lease.Close()

	var src resumeSnapshotSource
	actor, actorTemplate, src, err = w.loadActorForResume(leaseCtx, actorRef)
	if err != nil {
		return nil, false, err
	}
	if wasRunning = actor.GetStatus().GetState() == ateapipb.ActorState_ACTOR_STATE_RUNNING; wasRunning {
		return actor, false, nil
	}
	var created *ateapipb.Actor
	if created, err = w.ensureVolumesCreated(leaseCtx, actorRef, actor, actorTemplate); err != nil {
		return nil, false, err
	}
	actor = created
	var worker *ateapipb.Worker
	var assigned *ateapipb.Actor
	if assigned, worker, err = w.ensureWorkerAssigned(leaseCtx, actorRef, actor, actorTemplate); err != nil {
		return nil, false, err
	}
	actor = assigned
	if err = w.ensureVolumesAttached(leaseCtx, actor, worker, actorTemplate); err != nil {
		return nil, false, err
	}
	if tele, err = w.ensureAteletRestored(leaseCtx, actorRef, actor, actorTemplate, src); err != nil {
		return nil, false, err
	}
	var running *ateapipb.Actor
	if running, err = w.finalizeRunning(leaseCtx, actorRef); err != nil {
		return nil, false, err
	}
	actor = running
	return actor, true, nil
```

### Step: assign a worker

`ensureWorkerAssigned` retries `assignWorkerAttempt` with a short exponential backoff (about 3s) whenever it loses a race. Inside, everything from section 4 comes together. The scheduler picks a candidate from the cache, `BindActorToWorker` re-checks it under the row lock through the `admit` closure, and only then is the actor moved to `RESUMING` with its `WorkerAssignment`. No free worker surfaces as `ResourceExhausted`, which the router treats as a reason to *park* the request (section 9):

```bash
sed -n '488,536p' cmd/ateapi/internal/controlapi/workflow_resume.go
```

```output
		pickedWorker, err := w.scheduler.Schedule(ctx, constraints)
		if err != nil {
			if errors.Is(err, scheduling.ErrNoCapacity) {
				outcome = ateattr.SchedulerOutcomeNoFreeWorker
				return nil, nil, status.Errorf(codes.ResourceExhausted, "no free workers available")
			}
			return nil, nil, err
		}

		assignedWorker = pickedWorker
		slog.InfoContext(ctx, "Picked worker", slog.Any("worker", pickedWorker.String()))
	}

	assignment := &ateapipb.ActorAssignment{
		Actor: &ateapipb.ObjectRef{
			Atespace: actor.GetMetadata().GetAtespace(),
			Name:     actor.GetMetadata().GetName(),
		},
		ActorUid: actor.GetMetadata().GetUid(),
		// Record what this claim reserves so release returns the same amount.
		Resources: admittedResources(constraints),
	}
	assignment.ActorTemplateRef = actorTemplateObjectRef(actor)

	// The candidate came from a watch-fed cache, so it may already be full or no
	// longer eligible. The store re-asks under the Worker's row lock, where the
	// answer holds until the bind commits, so nothing here needs a fresh read
	// and two claims for the last place cannot both be admitted.
	admit := func(fresh *ateapipb.Worker) error {
		if !w.scheduler.Applies(fresh, constraints) || !w.scheduler.HasRoom(fresh, constraints) {
			return errWorkerFilledUp
		}
		return nil
	}
	if err := w.store.BindActorToWorker(ctx, assignedWorker.GetMetadata().GetName(), assignment, admit); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			w.workerCache.Forget(assignedWorker.GetMetadata().GetName())
			return nil, nil, fmt.Errorf("selected worker disappeared before claim: %w", store.ErrVersionConflict)
		}
		return nil, nil, err
	}

	newAssignment := workerAssignmentFrom(assignedWorker)
	storedActor, err := w.store.UpdateActor(ctx, actorRef, store.PreconditionFrom(actor), func(toUpdate *ateapipb.Actor) error {
		toUpdate.Status.State = ateapipb.ActorState_ACTOR_STATE_RESUMING
		toUpdate.Status.WorkerAssignment = newAssignment
		return nil
	})
	if err != nil {
```

### Step: tell atelet to restore

`ensureAteletRestored` dials the atelet on the assigned worker's **node** and calls one of three `AteomHerder` RPCs, depending on where the actor's state lives:

- **Local snapshot** (the actor was *paused*): `Restore` with type `LOCAL`. The files are still on that node's disk.
- **External snapshot** (the actor was suspended, or borrowed a tag): `Restore` with type `EXTERNAL` and a GCS/S3 URI.
- **No snapshot at all** (golden actors on first boot): `Run`, a cold boot from the template spec.

The request carries the workload spec, the resolved sandbox binaries (from the template's `SandboxConfig`), and resource limits. Here is the external-snapshot branch:

```bash
sed -n '724,774p' cmd/ateapi/internal/controlapi/workflow_resume.go
```

```output
	} else if !src.SnapshotURI.IsZero() {
		slog.InfoContext(ctx, "Actor has durable snapshot; Restoring from snapshot")
		// Mirrors loadActorForResume's source resolution: the durable URI is
		// the actor's own snapshot when one exists, the golden otherwise.
		tele.SnapshotKind = ateattr.SnapshotKindGolden
		if actor.GetStatus().GetExternalSnapshot().GetSnapshotUri() != "" {
			tele.SnapshotKind = ateattr.SnapshotKindLatest
		}
		var scope ateletpb.SnapshotScope
		var goldenSnapshotURI string
		switch {
		case src.TemplateReplaced:
			scope = ateletpb.SnapshotScope_SNAPSHOT_SCOPE_DATA
		case !src.GoldenSnapshotURI.IsZero():
			scope = ateletpb.SnapshotScope_SNAPSHOT_SCOPE_DATA_ON_GOLDEN
			goldenSnapshotURI = src.GoldenSnapshotURI.String()
		default:
			scope = actorSnapshotContentScopeToAtelet(src.Scope)
		}
		tele.WireSnapshotScope = ateattr.SnapshotScopeValue(scope)
		req := &ateletpb.RestoreRequest{
			TargetAteomUid:        assignment.GetWorkerPodUid(),
			Atespace:              actor.GetMetadata().GetAtespace(),
			ActorName:             actor.GetMetadata().GetName(),
			ActorTemplateAtespace: actor.GetActorTemplate().GetAtespace(),
			ActorTemplateName:     actor.GetActorTemplate().GetName(),
			Spec:                  workloadSpec,
			Type:                  ateletpb.CheckpointType_CHECKPOINT_TYPE_EXTERNAL,
			Config: &ateletpb.RestoreRequest_ExternalConfig{
				ExternalConfig: &ateletpb.ExternalCheckpointConfiguration{
					SnapshotUri: src.SnapshotURI.String(),
				},
			},
			Scope: scope,
			// Empty unless this is a Golden data resume.
			GoldenSnapshotUri: goldenSnapshotURI,
			SandboxAssets:     sandboxAssets,
			ActorUid:          actor.GetMetadata().Uid,
			EgressGateway:     egressGateway,
			CpuMilli:          cpuMilli,
			MemoryBytes:       memBytes,
		}
		if _, err = client.Restore(ctx, req); err != nil {
			slog.LogAttrs(ctx, slog.LevelError, "Setting Actor to crashed due to error",
				append(ateattr.ActorRefLogAttrs(actorRef), slog.Any("err", err))...)
			if cerr := crashActor(ctx, w.store, actorRef, ateattr.OperationResume, ateletCrashMessage("Restore", err)); cerr != nil {
				return tele, cerr
			}
			return tele, fmt.Errorf("actor %s crashed: %w", actorRef, err)
		}
		return tele, nil
```

The `SnapshotScope` values encode *what* to restore:

- `FULL`: memory plus the rootfs delta.
- `DATA`: only `DurableDir` volumes, followed by a cold boot. This is used when the template was replaced, since old memory would not match new code.
- `DATA_ON_GOLDEN`: the golden snapshot's memory with this actor's volume data laid over it.

### Rollback: crashActor

Any atelet failure goes through `crashActor` (`crash.go`). It is careful about ordering. It releases the worker *first*, and if that fails it leaves the assignment in place so a retry can release it again, rather than leak a worker slot forever:

```bash
sed -n '58,78p' cmd/ateapi/internal/controlapi/crash.go
```

```output
func crashActor(ctx context.Context, st crashActorStore, actorRef resources.ActorRef, opName, message string) error {
	actor, err := st.GetActor(ctx, actorRef)
	if err != nil {
		return fmt.Errorf("while loading actor to crash: %w", err)
	}

	wasAlreadyCrashed := actor.GetStatus().GetState() == ateapipb.ActorState_ACTOR_STATE_CRASHED
	opName = ateattr.NormalizeOperationName(opName)

	// Release the worker before moving the actor to CRASHED state.
	// If the release fails we must not clear the actor's worker assignment or
	// mark it CRASHED: doing so would strand the still-assigned worker with no
	// actor referencing it, so nothing would ever retry the release and the
	// worker slot would be consumed until its pod dies. Returning the error
	// instead leaves the actor (and its assignment) intact so the caller retries
	// crashActor, which re-attempts the release. releaseWorker is idempotent, so
	// a retry after a release that already succeeded is a no-op.
	sandboxClass, _, err := releaseWorker(ctx, st, actor)
	if err != nil {
		return fmt.Errorf("while releasing worker to crash actor: %w", err)
	}
```

On success, `finalizeRunning` re-reads the actor for a fresh version and commits `RUNNING`:

```bash
sed -n '811,833p' cmd/ateapi/internal/controlapi/workflow_resume.go
```

```output
// finalizeRunning re-reads the actor for a fresh version and commits RUNNING.
func (w *ActorWorkflow) finalizeRunning(ctx context.Context, actorRef resources.ActorRef) (_ *ateapipb.Actor, err error) {
	ctx, done := stepSpan(ctx, "FinalizeRunning")
	defer func() { err = done(err) }()

	latestActor, err := w.store.GetActor(ctx, actorRef)
	if err != nil {
		return nil, err
	}

	storedActor, err := w.store.UpdateActor(ctx, actorRef, store.PreconditionFrom(latestActor), func(toUpdate *ateapipb.Actor) error {
		toUpdate.Status.State = ateapipb.ActorState_ACTOR_STATE_RUNNING
		return nil
	})
	if err != nil {
		if errors.Is(err, store.ErrVersionConflict) {
			return nil, status.Error(codes.Aborted, "concurrent update conflict, please retry")
		}
		return nil, err
	}
	logActorStateChanged(ctx, storedActor, ateattr.OperationResume)
	return storedActor, nil
}
```

## 7. On the node: atelet

`atelet` (`cmd/atelet/main.go`) is a DaemonSet with two gRPC faces: `AteomHerder` over TCP/TLS for ateapi, and `AteomSupport` over a unix socket for the ateoms on its node. It runs as root with every capability dropped, so it never mounts anything itself. Mounting is ateom's job.

How does atelet find the ateom for a worker? Purely by pod UID. Every worker pod's ateom listens on a well-known socket path on a shared host directory (`internal/nodepath/nodepath.go`):

```bash
sed -n '62,86p' internal/nodepath/nodepath.go
```

```output
func AteomsDir() string {
	return filepath.Join(BasePath, "ateoms")
}

func AteomPath(podUID string) string {
	return filepath.Join(AteomsDir(), podUID)
}

func AteomSocketPath(podUID string) string {
	return filepath.Join(
		AteomPath(podUID),
		"ateom.sock",
	)
}

// ActorNetNSName names an actor's sandbox network namespace.
func ActorNetNSName(actorUID string) string {
	return "ateom-actor:" + actorUID
}

// ActorNetNSPath is the mount path of the actor's named namespace.
func ActorNetNSPath(actorUID string) string {
	return filepath.Join("/run/netns", ActorNetNSName(actorUID))
}
```

```bash
sed -n '1628,1648p' cmd/atelet/main.go
```

```output
func (d *AteomDialer) DialAteomPod(ctx context.Context, podUID string) (*grpc.ClientConn, error) {
	key := podUID

	connAny, ok := d.conns.Get(key)
	if ok {
		return connAny.(*grpc.ClientConn), nil
	}

	conn, err := grpc.NewClient(
		"unix://"+ateomSocketPath(podUID),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithStatsHandler(otelgrpc.NewClientHandler()),
	)
	if err != nil {
		return nil, fmt.Errorf("while creating atelet gRPC client connection: %w", err)
	}

	d.conns.Add(key, conn)

	return conn, nil
}
```

### atelet.Restore

`Restore` first fetches the **snapshot manifest**, a small JSON "sandbox record" stored next to the checkpoint files. It lists which files make up the snapshot and pins the exact sandbox runtime version that produced them:

```bash
sed -n '1038,1068p' cmd/atelet/main.go
```

```output
			dManifest = time.Since(tManifest)
		}
	}()
	var sandboxRec *sandboxAssetsRecord
	switch req.GetType() {
	case ateletpb.CheckpointType_CHECKPOINT_TYPE_EXTERNAL:
		uri, err := resources.ParseSnapshotURI(req.GetExternalConfig().GetSnapshotUri())
		if err != nil {
			return nil, err
		}
		manifestURI, err := uri.ObjectURI(sandboxManifestName)
		if err != nil {
			return nil, err
		}
		manifest, err := ategcs.FetchFromGCS(ctx, s.gcsClient, manifestURI)
		if err != nil {
			return nil, fmt.Errorf("while fetching snapshot manifest: %w", err)
		}
		if sandboxRec, err = unmarshalSandboxRecord(manifest); err != nil {
			return nil, fmt.Errorf("while unmarshalling sandbox record: %w", err)
		}
	case ateletpb.CheckpointType_CHECKPOINT_TYPE_LOCAL:
		manifest, err := os.ReadFile(filepath.Join(ateletpath.LocalSnapshotDir(actorUID, req.GetLocalConfig().GetSnapshotName()), sandboxManifestName))
		if err != nil {
			return nil, wrapFileSystemErr("while reading local snapshot manifest", err)
		}
		if sandboxRec, err = unmarshalSandboxRecord(manifest); err != nil {
			return nil, fmt.Errorf("while unmarshalling sandbox record: %w", err)
		}
	default:
		return nil, fmt.Errorf("unexpected checkpoint type: %v", req.GetType())
```

Then comes the main latency optimization. The snapshot download runs **concurrently** with fetching the sandbox binaries and unpacking the container image into an OCI bundle, because only the final ateom call needs both:

```bash
sed -n '1101,1120p;1162,1193p' cmd/atelet/main.go
```

```output

	// Undo the Register if the restore fails.
	defer func() {
		if err != nil {
			s.systemInfoVolumes.Deregister(actorUID)
		}
	}()

	// Download the memory snapshot and prepare the sandbox assets + OCI bundle
	// CONCURRENTLY. They are independent — only the final ateom.RestoreWorkload
	// needs both — so overlapping the GCS download (~0.5s warm) with the asset
	// fetch + image unpack hides whichever leg is shorter, and on a cold node
	// (uncached assets + image, ~2.5s unpack) that overlap is large.
	// TODO(dberkov): the old pause checkpoint files are not deleted after they are
	// copied to checkpointDir for the LOCAL case.
	var assetPaths map[string]string
	// One per leg: a single field written from both goroutines would race.
	var downloadErr, prepErr error
	var prepFailedPhase string
	g, gctx := errgroup.WithContext(ctx)
			if err := gLocal.Wait(); err != nil {
				return err
			}
		}
		return nil
	})
	g.Go(func() (err error) {
		defer func() { prepErr = err }()
		tAssets := time.Now()
		assetPaths, err = s.ensureSandboxAssets(gctx, runtimeRec)
		dAssets = time.Since(tAssets)
		if err != nil {
			prepFailedPhase = ateattr.SnapshotPhaseSandboxAssets
			return err
		}
		if err = s.systemInfoVolumes.Register(actorUID, actorRef, systemInfoVolumesFor(actorUID, req.GetSpec())); err != nil {
			prepFailedPhase = ateattr.SnapshotPhaseOCIUnpack
			return err
		}
		t := time.Now()
		err = s.prepareOCIBundles(gctx, actorUID, actorRef, req.GetSpec(), runtimeRec.PauseImage, req.GetTargetAteomUid())
		dBundles = time.Since(t)
		if err != nil {
			prepFailedPhase = ateattr.SnapshotPhaseOCIUnpack
			return err
		}
		return nil
	})
	if err := g.Wait(); err != nil {
		if isCollateral(err, downloadErr) {
			dDownload = 0
		}
```

Snapshot files move through `cmd/atelet/internal/ategcs`, which puts GCS and S3 behind one `ObjectStorage` interface. Each file is stored as `<name>.zstd` and all of them are fetched in parallel:

```bash
sed -n '1395,1419p' cmd/atelet/main.go
```

```output
func (s *AteomHerder) downloadExternalCheckpoint(ctx context.Context, snapshotURI string, dstDir string, files []string) error {
	uri, err := resources.ParseSnapshotURI(snapshotURI)
	if err != nil {
		return err
	}
	g, gCtx := errgroup.WithContext(ctx)
	for _, fileName := range files {
		fileName := fileName
		local := filepath.Join(dstDir, fileName)
		g.Go(func() error {
			objectURI, err := uri.ObjectURI(fileName + ".zstd")
			if err != nil {
				return fmt.Errorf("while addressing %s in GCS: %w", fileName, err)
			}
			if err := ategcs.FetchLocalFileFromGCSWithZstd(gCtx, s.gcsClient, objectURI, local); err != nil {
				return fmt.Errorf("while downloading %s from GCS: %w", fileName, err)
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return err
	}

	return nil
```

### Building OCI bundles without mounting

The other leg, `prepareOCIDirectory` (`cmd/atelet/oci.go`), produces one OCI bundle per container: a hidden "pause" container that holds the sandbox's namespaces, plus each app container. Because atelet has no capabilities, it only creates *empty* `rootfs/upper/work` directories and writes two JSON files. One is `rootfs-overlay.json`, listing the image layers to stack. The other is the OCI `config.json`, which points the container at the actor's network namespace. ateom does the actual overlay mount later.

```bash
sed -n '92,104p;145,166p' cmd/atelet/oci.go
```

```output

	// The bundle's rootfs is composed by ateom as an overlay mount just before
	// the workload runs: the cached image layers are the read-only lowerdirs,
	// and the bundle-local upper/work hold this actor's private writes (wiped
	// between runs, preserving the pristine-rootfs-per-run contract the old
	// full re-untar provided). atelet only prepares the (empty) directories —
	// it deliberately runs with no capabilities, so it cannot mount.
	for _, d := range []string{"rootfs", "upper", "work"} {
		if err := os.MkdirAll(path.Join(bundlePath, d), 0o700); err != nil {
			return fmt.Errorf("in os.MkdirAll for container bundle dir: %w", err)
		}
	}

		ImageVolumes: imageVolumes,
	}); err != nil {
		return fmt.Errorf("while writing overlay spec: %w", err)
	}

	// Write the runtime-neutral OCI spec to config.json.
	if err := ocispec.Save(bundlePath, ocispec.Build(ocispec.Options{
		Args:                      resolvedArgs,
		Env:                       resolvedEnv,
		NetNSPath:                 netns,
		Volumes:                   volumes,
		VolumeMounts:              volumeMounts,
		Capabilities:              capabilities,
		Resources:                 resources,
		DurableDirVolumeMountsDir: ateletpath.DurableDirVolumeMountsDir(actorUID),
		VolumesDir:                ateletpath.VolumesDir(actorUID),
		SystemInfoVolumeRootsDir:  ateletpath.SystemInfoVolumeRootsDir(actorUID),
		BundlePath:                bundlePath,
	})); err != nil {
		return fmt.Errorf("while writing OCI spec: %w", err)
	}

```

### The image cache

`imagecache.EnsureImage` pulls images into a node-local, content-addressed pool of unpacked layers shared by every worker on the node. Two concurrent resumes of the same image share a single pull (singleflight), and a layer is published by atomic rename, so an existing layer directory is always complete. The package doc describes the split of duties: atelet runs the unprivileged store, and ateom runs the privileged mount:

```bash
sed -n '15,46p' internal/imagecache/imagecache.go
```

```output
// Package imagecache implements the node-local OCI image cache: a
// content-addressed pool of unpacked image layers shared by every actor on
// the node, plus the per-bundle overlay spec that tells the ateom runtimes
// how to compose an actor rootfs from cached layers.
//
// The work is split along the existing atelet/ateom privilege boundary:
//
//   - atelet (plain root, all capabilities dropped) pulls layers and unpacks
//     them into the pool (Store.EnsureImage), and writes a rootfs-overlay.json
//     next to each bundle's config.json (WriteSpec). Whiteout entries are
//     recorded in per-layer metadata rather than materialized, because
//     overlayfs whiteouts are char devices (CAP_MKNOD) with trusted.* xattrs
//     for opaque dirs (CAP_SYS_ADMIN).
//   - ateom (privileged; it already owns every mount on the node) finalizes
//     layers — materializing the recorded whiteout state, once per layer —
//     and mounts the overlay rootfs (SetupBundleRootfs) just before
//     `runsc create` / staging the micro-VM virtio-fs lower.
//
// On-disk layout under the cache root (a directory on the BasePath hostPath,
// so the same absolute paths resolve in atelet and every ateom pod):
//
//	version                          layout version marker
//	layers/sha256/<diffid-hex>/
//	    fs/                          the unpacked layer tree (overlay lowerdir)
//	    whiteouts.json               whiteout state recorded at unpack time
//	    finalized                    marker written by FinalizeLayer (ateom)
//	manifests/sha256/<digest-hex>.json
//	                                 image config + ordered diffID list
//
// Layers land in the pool via unpack-into-tempdir + atomic rename, so a
// layer directory that exists is always complete; startup recovery only has
// to sweep orphaned temp dirs.
```

With both legs done, atelet hands off to ateom over the unix socket. Note `RunscPath`: the gVisor binary is not baked into the worker image. It comes from the `SandboxConfig` and is pinned into every snapshot manifest, so a restore uses the same runsc that took the checkpoint:

```bash
sed -n '1214,1232p' cmd/atelet/main.go
```

```output
	tAteom := time.Now()
	_, err = client.RestoreWorkload(ctx, &ateompb.RestoreWorkloadRequest{
		Atespace:              actorRef.Atespace,
		ActorName:             actorRef.Name,
		ActorTemplateAtespace: req.GetActorTemplateAtespace(),
		ActorTemplateName:     req.GetActorTemplateName(),
		RunscPath:             runscPathFor(assetPaths),
		RuntimeAssetPaths:     assetPaths,
		Spec:                  spec,
		Scope:                 toAteomSnapshotScope(req.GetScope()),
		ActorUid:              req.GetActorUid(),
		ActorDirs:             ateletpath.ActorDirs(actorUID),
		EgressGateway:         toAteomEgressGateway(req.GetEgressGateway()),
		CpuMilli:              req.GetCpuMilli(),
		MemoryBytes:           req.GetMemoryBytes(),
		// Informational: for DATA_ON_GOLDEN the golden snapshot's files are
		// already staged into the restore dir by the combined download above;
		// ateom restores from the shared dir and never fetches this URI.
		GoldenSnapshotUri: req.GetGoldenSnapshotUri(),
```

## 8. Inside the worker pod: ateom-gvisor

`ateom-gvisor` (`cmd/ateom-gvisor/main.go`) is the privileged process in each worker pod. At startup it listens on the unix socket from `nodepath.AteomSocketPath(podUID)`, starts atunnel's listeners, and reports its capacity to atelet over `AteomSupport`.

`RestoreWorkload` spells out its contract with atelet in a comment: binaries, bundles, and checkpoint files are already on disk. Its job is to build networking (`hostActor`), mount each bundle's overlay rootfs, and run `runsc create` followed by `runsc restore` for the pause container and then each app container. With `DATA` scope it cold-starts instead (`create` + `start`):

```bash
sed -n '904,916p;947,980p' cmd/ateom-gvisor/main.go
```

```output

	// Contract with atelet:
	//
	//   * Correct runsc version is downloaded and placed on disk.
	//   * All OCI bundles are set up, including for the pause container.
	//   * Checkpoint downloaded and placed on disk

	egress, err := s.prepareActorEgress(ctx, req.GetAtespace(), req.GetActorName(), req.GetActorUid(), req.GetEgressGateway())
	if err != nil {
		return nil, err
	}
	if _, err := s.hostActor(ctx, attribution); err != nil {
		return nil, err
			return nil, fmt.Errorf("while restoring durable-dir volumes: %w", err)
		}
	}
	// Compose the pause rootfs before create (see RunWorkload). runsc restore
	// only needs the rootfs to hold the correct content; whether it came from
	// an untar or an overlay of cached layers is transparent to it.
	if err := imagecache.SetupBundleRootfs(ateompath.OCIBundlePath(req.GetActorUid(), ocispec.PauseContainer)); err != nil {
		return nil, fmt.Errorf("while composing pause rootfs: %w", err)
	}

	switch req.GetScope() {
	case ateompb.SnapshotScope_SNAPSHOT_SCOPE_DATA:
		// Create and start pause container (cold boot with durable-dir volumes restored)
		containersToDelete = append(containersToDelete, ocispec.PauseContainer)
		if err := rcmd.cmdCreate(ctx, os.Stdout, ocispec.PauseContainer, nil); err != nil {
			return nil, fmt.Errorf("while creating pause container: %w", err)
		}
		if err := rcmd.cmdStart(ctx, os.Stdout, ocispec.PauseContainer); err != nil {
			return nil, fmt.Errorf("while starting pause container: %w", err)
		}
	case ateompb.SnapshotScope_SNAPSHOT_SCOPE_FULL, ateompb.SnapshotScope_SNAPSHOT_SCOPE_DATA_ON_GOLDEN:
		// Create and restore pause container
		containersToDelete = append(containersToDelete, ocispec.PauseContainer)
		if err := rcmd.cmdCreate(ctx, os.Stdout, ocispec.PauseContainer, nil); err != nil {
			return nil, fmt.Errorf("while creating pause container: %w", err)
		}
		if err := rcmd.cmdRestore(ctx, os.Stdout, ocispec.PauseContainer, checkpointDir); err != nil {
			return nil, fmt.Errorf("while restoring pause container: %w", err)
		}
	default:
		return nil, fmt.Errorf("unexpected snapshot scope: %v", req.GetScope())
	}

	// Create and restore each application container, each with its own log pipe so
```

The runsc invocations live in `cmd/ateom-gvisor/runsc.go`. Here is `restore`. `--cpu-num-from-quota` sizes the gVisor sentry to the actor's CPU limit rather than the whole host, and `-image-path` points at the directory atelet downloaded the snapshot into:

```bash
sed -n '265,300p' cmd/ateom-gvisor/runsc.go | grep -v '^\s*// "-'
```

```output
func (r *runsc) cmdRestore(ctx context.Context, out io.Writer, containerName, checkpointPath string) error {
	slog.InfoContext(ctx, "About to run runsc restore", slog.String("container", containerName))

	if err := r.shapeSpec(containerName); err != nil {
		return fmt.Errorf("while shaping the OCI spec for %q: %w", containerName, err)
	}

	restoreArgs := []string{
		"-log-format", "json",
		"--alsologtostderr",
		"-root", ateompath.RunSCStateDir(r.actorUID),
		// Match cmdCreate: size the restored sentry from the cgroup CPU quota.
		"--cpu-num-from-quota",
	}
	restoreArgs = append(restoreArgs,
		"restore",
		"-bundle", ateompath.OCIBundlePath(r.actorUID, containerName),
		"-image-path", checkpointPath,
		"-pid-file", ateompath.PIDFilePath(r.actorUID, containerName),
		"-background",
		"-detach",
		containerName,
	)
	cmd := exec.CommandContext(ctx, r.path, restoreArgs...)
	cmd.Stdout = out
	cmd.Stderr = out
	if err := reaper.RunCommand(cmd); err != nil {
		return fmt.Errorf("while running `runsc restore`: %w", err)
	}
	return nil
}
```

The resume only finishes once the actor is actually ready. `wakeupprobe.WaitAll` blocks until every container with a wakeup probe answers 200. Then `activateActorNetworking` tells atunnel to start routing requests for this actor:

```bash
sed -n '1009,1024p' cmd/ateom-gvisor/main.go
```

```output
			return nil, fmt.Errorf("unexpected snapshot scope: %v", req.GetScope())
		}
	}

	// Block until every wakeup-probe-enabled container reports 200.
	if err := wakeupprobe.WaitAll(ctx, req.GetSpec().GetContainers(), ateomnet.ActorVethIP, wakeupprobe.DialFunc(s.sandboxDialer(req.GetActorUid()))); err != nil {
		return nil, fmt.Errorf("while waiting for container wakeup probe: %w", err)
	}
	if err := s.activateActorNetworking(ateomstats.ActorAttributionFromRequest(req), egress); err != nil {
		return nil, err
	}

	s.actorLogger.EmitLifecycleLog(ctx, "Actor restored", attribution)
	s.setSession(req.GetActorUid(), &workloadSession{rcmd: rcmd, containers: containerNames(req.GetSpec().GetContainers())})

	return &ateompb.RestoreWorkloadResponse{}, nil
```

### Sandbox networking

Every actor gets the *same* fixed address, because each sandbox has its own private network namespace (`internal/ateomnet/net.go`):

```bash
sed -n '37,48p' internal/ateomnet/net.go
```

```output
	ActorVethName    = "eth0"
	ActorVethGateway = "169.254.17.1"
	ActorVethIP      = "169.254.17.2"

	// hostVethLocalAddress is the gateway interface's IP address and prefix length.
	hostVethLocalAddress = "169.254.17.1/30"
	// actorVethLocalAddress is the actor interface's IP address and prefix length.
	actorVethLocalAddress = "169.254.17.2/30"

	// ActorVethSubnet is the point-to-point /30 the actor veth lives on.
	ActorVethSubnet = "169.254.17.0/30"
)
```

`setupVethPair` (`internal/ateomnet/sandbox.go`) creates two namespaces: the actor's netns, which gVisor imports into its own network stack, and a "gateway" netns next to it where ateom's atunnel lives. A veth pair joins them, with the peer created directly inside the actor namespace, which avoids a costly move across namespaces on the resume path:

```bash
sed -n '144,163p' internal/ateomnet/sandbox.go
```

```output
	if err := NetNSDo(ctx, outer, func(context.Context) error {
		veth := &netlink.Veth{
			LinkAttrs: netlink.LinkAttrs{Name: gatewayVethName},
			PeerName:  ActorVethName,
			// Create the peer directly in the actor's namespace. Moving a
			// netdev across namespaces afterwards costs several times the
			// whole setup, all of it under the global RTNL lock, and this
			// runs on the resume path.
			PeerNamespace: netlink.NsFd(int(actorNS)),
		}
		if err := netlink.LinkAdd(veth); err != nil {
			return fmt.Errorf("while creating the veth pair: %w", err)
		}
		atSide, err := netlink.LinkByName(gatewayVethName)
		if err != nil {
			return err
		}
		if err := netlink.AddrReplace(atSide, HostVethAddr); err != nil {
			return fmt.Errorf("while assigning the atunnel-side address: %w", err)
		}
```

Outbound traffic is captured with one nftables rule. Any TCP packet from the actor not addressed to its own /30 is `REDIRECT`ed to atunnel's egress port, and the original destination is preserved for the egress policy check:

```bash
sed -n '235,257p' internal/ateomnet/sandbox.go
```

```output
	prerouting := c.AddChain(&nftables.Chain{
		Name: "prerouting", Table: table, Type: nftables.ChainTypeNAT,
		Hooknum: nftables.ChainHookPrerouting, Priority: nftables.ChainPriorityNATDest,
	})

	// Offset of the destination address in an IPv4 header.
	const ipv4HeaderDst = 16
	exprs := []expr.Any{
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: ipv4HeaderDst, Len: 4},
		&expr.Bitwise{
			SourceRegister: 1, DestRegister: 1, Len: 4,
			Mask: actorSubnetMask, Xor: []byte{0, 0, 0, 0},
		},
		&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: actorSubnetBase},
	}
	exprs = append(exprs, l4ProtocolEqual(unix.IPPROTO_TCP)...)
	exprs = append(exprs,
		&expr.Immediate{Register: 1, Data: binaryutil.BigEndian.PutUint16(egressPort)},
		&expr.Redir{RegisterProtoMin: 1},
	)
	c.AddRule(&nftables.Rule{Table: table, Chain: prerouting, Exprs: exprs})

	if err := c.Flush(); err != nil {
```

## 9. A request arrives: atenet router → atunnel → actor

Now the actor can be woken by traffic. A client (usually a higher-level system) sends an ordinary HTTP request to the `atenet` router with one extra header, parsed in `internal/atenet/headers.go`:

```bash
sed -n '25,40p' internal/atenet/headers.go
```

```output
const (
	// TargetActorHeader identifies the actor selected for ingress routing as
	// "<atespace>/<actor>". HTTP field names are case-insensitive; this uses its
	// HTTP/2 wire form so dataplane configuration and metadata are native.
	TargetActorHeader = "ate-target-actor"
)

// ParseTargetActor parses and validates a TargetActorHeader value.
func ParseTargetActor(value string) (resources.ActorRef, error) {
	atespace, actorName, ok := strings.Cut(value, "/")
	if !ok || strings.Contains(actorName, "/") ||
		!resources.IsValidResourceName(atespace) || !resources.IsValidResourceName(actorName) {
		return resources.ActorRef{}, fmt.Errorf("invalid actor reference %q", value)
	}
	return resources.ActorRef{Atespace: atespace, Name: actorName}, nil
}
```

The router is Envoy configured by an in-process xDS server (`cmd/atenet/internal/router/xds.go`), plus an `ext_proc` gRPC server that Envoy consults on every request's headers. The ingress handler (`cmd/atenet/internal/router/ingress/ingress.go`) is where "route to an actor" becomes "resume the actor":

1. Parse `ate-target-actor`. For a CONNECT request, also pick up the target port from the outer authority.
2. Call `ResumeActor`. This is the fast path when the actor is already running, and a full restore otherwise.
3. Put `workerPodIP:443` into Envoy **dynamic metadata** for an `ORIGINAL_DST` cluster.
4. Overwrite the routing header so the client cannot swap actors after resolution.

```bash
sed -n '105,151p' cmd/atenet/internal/router/ingress/ingress.go
```

```output
			targetPort = p
		}
	}

	slog.InfoContext(ctx, "ResumeActor", slog.Any("actor", actorRef))
	actor, resumeOutcome, err := h.resumer.ResumeActor(ctx, actorRef)
	if err != nil {
		return extproc.Result{Resume: string(resumeOutcome)}, mapResumeError(actorRef, err)
	}

	// ActorTemplate reference, used as low-cardinality route-latency metric
	// attributes.
	res := extproc.Result{
		TemplateAtespace: actor.GetActorTemplate().GetAtespace(),
		TemplateName:     actor.GetActorTemplate().GetName(),
		Resume:           string(resumeOutcome),
	}

	workerIP := actor.GetStatus().GetWorkerAssignment().GetWorkerPodIp()
	slog.InfoContext(ctx, "ResumeActor result",
		slog.Any("actor", actorRef),
		slog.String("state", actor.GetStatus().GetState().String()),
		slog.String("workerIP", workerIP))

	if ip := net.ParseIP(workerIP); ip == nil {
		return res, extproc.NewReqError(envoy_type.StatusCode_InternalServerError,
			"actor %s routing failed", actorRef)
	}

	// atunnel's regular HTTPS ingress listens on :443 and forwards to the
	// actor's targetPort.
	targetAddr := net.JoinHostPort(workerIP, "443")

	slog.InfoContext(ctx, "Route ok", slog.Any("actor", actorRef), slog.String("targetAddr", targetAddr))

	// ext_proc clients may use regular HTTPS on :443 or choose to CONNECT to
	// atunnel instead.
	dynamicMetadata, err := structpb.NewStruct(map[string]any{
		OriginalDstMetadataKey: map[string]any{
			OriginalDstAddressKey: targetAddr,
			OriginalDstPortKey:    strconv.Itoa(targetPort),
		},
	})
	if err != nil {
		return res, extproc.NewReqError(envoy_type.StatusCode_InternalServerError,
			"actor %s routing failed", actorRef)
	}
```

### Request coalescing and parking

A burst of requests for one suspended actor must not trigger a burst of resumes. `ActorResumer` (`ingress/resumer.go`) keys a shared **flight** by actor: the first caller starts it, and later callers join. The flight is detached from any one caller's cancellation, so a client disconnecting cannot abort a resume that others are waiting on:

```bash
sed -n '194,211p' cmd/atenet/internal/router/ingress/resumer.go
```

```output
	// (step.*, atelet, snapshot fetch) fragment away from the request that
	// triggered them and sample independently of it. Joiners share the
	// leader's flight, so the flight carries the leader's span context.
	callerSpanCtx := trace.SpanContextFromContext(ctx)

	key := actorRef.String()
	r.mu.Lock()
	f, ok := r.flights[key]
	if !ok {
		f = newResumeActorFlight()
		r.flights[key] = f
		go r.runFlight(f, key, actorRef, reqID, callerSpanCtx)
	}
	r.mu.Unlock()

	return r.awaitFlight(ctx, f, actorRef, reqID)
}

```

Inside the flight, errors that mean "not yet" are retried with exponential backoff under a time budget. That includes `ResourceExhausted`, which is what ateapi returned above when no worker was free. The request is **parked** rather than failed. `docs/request-parking.md` sums up the semantics as "served late, never canceled", and a bounded parking lot (`ingress/parking.go`) turns overflow into a 503:

```bash
sed -n '171,180p;225,245p' cmd/atenet/internal/router/ingress/resumer.go
```

```output
func (r *ActorResumer) retryable(err error) bool {
	switch status.Code(err) {
	case codes.Aborted:
		return true
	case codes.ResourceExhausted, codes.FailedPrecondition, codes.Unavailable:
		return r.parkEnabled
	default:
		return false
	}
}
	var lastRetryErr error

	err := wait.ExponentialBackoffWithContext(bgCtx, backoff, func(context.Context) (bool, error) {
		var err error
		resumeResp, err = r.apiClient.ResumeActor(attemptCtx, &ateapipb.ResumeActorRequest{
			Actor: actorRef.ToObjectRef(),
		})
		if err == nil {
			return true, nil
		}

		if r.retryable(err) {
			f.signalRetrying()
			lastRetryErr = err // remember it in case the budget elapses
			return false, nil  // park: retry until the budget elapses
		}
		return false, err
	})

	r.publish(f, key, flightResult(bgCtx, resumeResp, err, lastRetryErr, reqID))
}
```

### Envoy follows the metadata

Envoy never works out the destination from the Host header. The router's single catch-all route sends everything to an `ORIGINAL_DST` cluster that reads its address from the dynamic metadata key ext_proc just set. It also copies the actor's port into a header for atunnel. The upstream connection is mTLS, with the worker verified by SPIFFE URI rather than by IP (`buildUpstreamTransportSocket`):

```bash
sed -n '793,811p;866,875p' cmd/atenet/internal/router/xds.go
```

```output
func (x *XdsServer) buildOriginalDstCluster() *clusterv3.Cluster {
	cluster := &clusterv3.Cluster{
		Name:           OriginalDstClusterName,
		ConnectTimeout: durationpb.New(5 * time.Second),
		ClusterDiscoveryType: &clusterv3.Cluster_Type{
			Type: clusterv3.Cluster_ORIGINAL_DST,
		},
		LbPolicy: clusterv3.Cluster_CLUSTER_PROVIDED,
		LbConfig: &clusterv3.Cluster_OriginalDstLbConfig_{
			OriginalDstLbConfig: &clusterv3.Cluster_OriginalDstLbConfig{
				MetadataKey: &metadatav3.MetadataKey{
					Key: ingress.OriginalDstMetadataKey,
					Path: []*metadatav3.MetadataKey_PathSegment{
						{Segment: &metadatav3.MetadataKey_PathSegment_Key{Key: ingress.OriginalDstAddressKey}},
					},
				},
			},
		},
	}
						RequestHeadersToAdd: []*corev3.HeaderValueOption{
							{
								Header: &corev3.HeaderValue{
									Key: atunnel.TargetPortHeader,
									Value: fmt.Sprintf("%%DYNAMIC_METADATA(%s:%s)%%",
										ingress.OriginalDstMetadataKey, ingress.OriginalDstPortKey),
								},
								AppendAction: corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD,
							},
						},
```

### atunnel: the last hop

Inside the worker pod, atunnel (`internal/atunnel/ingress.go`, hosted by ateom) terminates that mTLS connection. It accepts **only** the router's SPIFFE identity:

```bash
sed -n '152,163p' internal/atunnel/ingress.go
```

```output
			if len(cs.PeerCertificates) == 0 {
				return fmt.Errorf("atunnel: client certificate is required")
			}
			for _, uri := range cs.PeerCertificates[0].URIs {
				if uri.String() == cfg.AllowedClientID {
					return nil
				}
			}
			return fmt.Errorf("atunnel: client is not %q", cfg.AllowedClientID)
		},
	}
	return s, nil
```

Then it checks that the named actor is *currently active on this worker*. This is the map that `activateActorNetworking` filled at the end of `RestoreWorkload`. If the actor is not active here, atunnel answers `421 Misdirected Request` with `X-Ate-Assignment-Stale`, which tells the router its assignment is out of date:

```bash
sed -n '519,557p' internal/atunnel/ingress.go
```

```output
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	active, requestCtx, release, ok := s.authorize(r)
	if !ok {
		s.reject(w)
		return
	}
	defer release()

	active.proxy.ServeHTTP(w, r.WithContext(requestCtx))
}

func (s *Server) authorize(r *http.Request) (*activation, context.Context, func(), bool) {
	ref, err := atenet.ParseTargetActor(r.Header.Get(atenet.TargetActorHeader))
	if err != nil {
		return nil, nil, nil, false
	}

	s.mu.Lock()
	active, ok := s.active[ref]
	if !ok {
		s.mu.Unlock()
		return nil, nil, nil, false
	}
	active.wg.Add(1)
	s.mu.Unlock()
	requestCtx, cancel := context.WithCancel(r.Context())
	stop := context.AfterFunc(active.ctx, cancel)
	release := func() {
		active.wg.Done()
		stop()
		cancel()
	}
	return active, requestCtx, release, true
}

func (s *Server) reject(w http.ResponseWriter) {
	w.Header().Set(StaleAssignmentHeader, "true")
	http.Error(w, "misdirected request", http.StatusMisdirectedRequest)
}
```

If the actor is active, a reverse proxy rewrites the request to `http://169.254.17.2:<target port>` and dials it through the veth pair into the gVisor sandbox. The request has arrived.

## 10. Going back to sleep: suspend, pause and delete

Suspend reverses the resume path. `SuspendActor` has the same structure as `ResumeActor`: take the per-actor lease, then run idempotent `ensureX` steps. Each step checks the stored state first, so a retried call picks up where the last one stopped. If the actor is already SUSPENDED, the call returns right away.

```bash
sed -n '58,103p' cmd/ateapi/internal/controlapi/workflow_suspend.go
```

```output
	leaseCtx, lease, err := w.acquireActorLease(ctx, actorRef)
	if err != nil {
		return nil, err
	}
	defer lease.Close()

	actor, actorTemplate, err = w.loadActorForSuspend(leaseCtx, actorRef)
	if err != nil {
		return nil, err
	}
	if actor.GetStatus().GetState() == ateapipb.ActorState_ACTOR_STATE_SUSPENDED {
		// Fully suspended already: FinalizeSuspended commits SUSPENDED and the
		// cleared worker assignment in a single update, so there is nothing
		// left to do. This success reports no pool, and cannot: the previous
		// attempt released the worker, so the record names none (#957).
		return actor, nil
	}
	// Decided before marking: once SUSPENDING is committed, the loaded status
	// alone can no longer tell the two origins apart.
	fromPaused := isPausedOriginSuspend(actor)
	var marked *ateapipb.Actor
	if marked, err = w.ensureMarkedSuspending(leaseCtx, actorRef, actor, actorTemplate); err != nil {
		return nil, err
	}
	actor = marked
	if fromPaused {
		wireSnapshotScope, err = w.ensurePausedSnapshotUploaded(leaseCtx, actorRef, actor, actorTemplate)
	} else {
		wireSnapshotScope, err = w.ensureAteletSuspended(leaseCtx, actorRef, actor, actorTemplate)
	}
	if err != nil {
		return nil, err
	}
	if err = w.ensureVolumesDetached(leaseCtx, actor, actorTemplate, "DetachVolumes", ateattr.OperationSuspend); err != nil {
		return nil, err
	}
	// FinalizeSuspended clears the WorkerAssignment the labels read, so snapshot
	// them here, as crash.go does for the crash counter.
	finalAttrs = lifecycleOpAttrs(actor, actorTemplate, "", wireSnapshotScope)
	var finalized *ateapipb.Actor
	if finalized, err = w.ensureSuspendedFinalized(leaseCtx, actorRef, actorTemplate); err != nil {
		return nil, err
	}
	actor = finalized
	return actor, nil
}
```

`ensureMarkedSuspending` commits SUSPENDING and reserves a fresh `InProgressSnapshotUri`. `ensureAteletSuspended` then sends an EXTERNAL `Checkpoint` to that worker's atelet. As on the resume path, if atelet fails the actor is crashed rather than left half-suspended:

```bash
sed -n '230,262p' cmd/ateapi/internal/controlapi/workflow_suspend.go
```

```output
		return "", err
	}

	// Checkpoint does not carry the sandbox config: atelet uses the version the
	// actor is currently running (recorded on-node at Run/Restore) and pins it
	// into the snapshot manifest.
	req := &ateletpb.CheckpointRequest{
		TargetAteomUid:        assignment.GetWorkerPodUid(),
		Atespace:              actor.GetMetadata().GetAtespace(),
		ActorName:             actor.GetMetadata().GetName(),
		ActorTemplateAtespace: actor.GetActorTemplate().GetAtespace(),
		ActorTemplateName:     actor.GetActorTemplate().GetName(),
		Spec:                  workloadSpec,
		Type:                  ateletpb.CheckpointType_CHECKPOINT_TYPE_EXTERNAL,
		Config: &ateletpb.CheckpointRequest_ExternalConfig{
			ExternalConfig: &ateletpb.ExternalCheckpointConfiguration{
				SnapshotUri: actor.GetStatus().GetInProgressSnapshotUri(),
			},
		},
		Scope:    actorSnapshotContentScopeToAtelet(commitSnapshotScope(actor.GetMetadata().GetAtespace(), actorTemplate)),
		ActorUid: actor.GetMetadata().Uid,
	}
	wireSnapshotScope = ateattr.SnapshotScopeValue(req.Scope)

	if _, err = client.Checkpoint(ctx, req); err != nil {
		slog.LogAttrs(ctx, slog.LevelError, "Setting Actor to crashed due to error",
			append(ateattr.ActorRefLogAttrs(actorRef), slog.Any("err", err))...)
		if cerr := crashActor(ctx, w.store, actorRef, ateattr.OperationSuspend, ateletCrashMessage("Checkpoint", err)); cerr != nil {
			return wireSnapshotScope, cerr
		}
		return wireSnapshotScope, fmt.Errorf("actor %s crashed: %w", actorRef, err)
	}
	return wireSnapshotScope, nil
```

On the node, atelet forwards the call to ateom as `CheckpointWorkload`. ateom first cuts the actor's networking, so no request can land mid-snapshot. It then runs `runsc checkpoint` on the pause container, which is the root of the sandbox. A FULL snapshot captures the process state, plus a tarball of any durable-dir volumes. A DATA snapshot pauses the sandbox and saves only the durable dirs. Afterwards ateom tears the containers down and reports exactly which files runsc wrote:

```bash
sed -n '729,810p' cmd/ateom-gvisor/main.go
```

```output
	if err := s.deactivateActorNetworking(ctx, ateomstats.ActorAttributionFromRequest(req)); err != nil {
		return nil, err
	}

	attribution := ateomstats.ActorAttributionFromRequest(req)
	s.actorLogger.EmitLifecycleLog(ctx, "Actor checkpointing", attribution)

	// Contract with atelet:
	//
	//   * After we exit, atelet will upload checkpoint to GCS
	//   * After we exit, atelet will tear down OCI bundles and reset the actor directory.

	// Checkpoint only saves state; no sizing is applied, so size is left zero.
	rcmd := &runsc{
		path:     req.GetRunscPath(),
		actorUID: req.GetActorUid(),
	}

	checkpointPath := ateompath.CheckpointStateDir(req.GetActorUid())
	if err := os.MkdirAll(checkpointPath, 0o700); err != nil {
		return nil, fmt.Errorf("while creating checkpoint directory: %w", err)
	}

	// Always take durable-dir snapshot if at least one container has a durable-dir volume mount.
	// TODO(dberkov): this is a temporary workaround until gVisor supports taking durable-dir snapshots in a single request with the process snapshot.
	switch req.GetScope() {
	case ateompb.SnapshotScope_SNAPSHOT_SCOPE_DATA:
		if !hasDurableVolumes(req.GetSpec().GetContainers()) {
			return nil, fmt.Errorf("no durable-dir volumes found for DATA snapshot")
		}
		if err := rcmd.cmdPause(ctx, ocispec.PauseContainer); err != nil {
			return nil, fmt.Errorf("while pausing pause container: %w", err)
		}
		tarErr := tarDurableVolumes(ctx, ateompath.DurableDirVolumeMountsDir(req.GetActorUid()), checkpointPath)
		// Undoing our own pause must not depend on the caller's context:
		// tarutil does not check ctx, so a deadline expiring mid-tar would
		// fail the resume instantly and leave the sandbox paused forever.
		resumeCtx, cancelResume := context.WithTimeout(context.WithoutCancel(ctx), resumeTimeout)
		defer cancelResume()
		if err := rcmd.cmdResume(resumeCtx, ocispec.PauseContainer); err != nil {
			return nil, fmt.Errorf("while resuming pause container: %w", err)
		}
		if tarErr != nil {
			return nil, fmt.Errorf("while archiving durable-dir volumes: %w", tarErr)
		}
	case ateompb.SnapshotScope_SNAPSHOT_SCOPE_FULL:
		// Checkpoint pause container (root of the sandbox)
		// TODO: Consider pause -> tar -> resume -> checkpoint order for better failure handling.
		if err := rcmd.cmdCheckpoint(ctx, ocispec.PauseContainer, checkpointPath); err != nil {
			return nil, fmt.Errorf("while checkpointing pause: %w", err)
		}
		if hasDurableVolumes(req.GetSpec().GetContainers()) {
			if err := tarDurableVolumes(ctx, ateompath.DurableDirVolumeMountsDir(req.GetActorUid()), checkpointPath); err != nil {
				return nil, fmt.Errorf("while archiving durable-dir volumes: %w", err)
			}
		}
	default:
		return nil, fmt.Errorf("unsupported snapshot scope: %v", req.GetScope())
	}

	// Cleanup the containers after checkpointing. This also unhosts the actor,
	// before the snapshot listing below can fail.
	// This is best-effort cleanup for actor containers that may have been left behind after checkpointing.
	if err := s.terminateWorkload(ctx, attribution.Ref, attribution.UID, req.GetRunscPath(), req.GetSpec().GetContainers()); err != nil {
		slog.WarnContext(ctx, "failed to terminate workload after checkpoint",
			slog.String("actor", attribution.Ref.String()),
			slog.String("actorUID", attribution.UID),
			slog.Any("err", err))
	}

	// Report exactly the files runsc wrote so atelet ships precisely this set
	// (checkpoint.img plus any pages images), rather than a hardcoded list.
	snapshotFiles, err := listSnapshotFiles(checkpointPath)
	if err != nil {
		return nil, fmt.Errorf("while listing checkpoint files: %w", err)
	}

	s.actorLogger.EmitLifecycleLog(ctx, "Actor checkpointed", attribution)

	return &ateompb.CheckpointWorkloadResponse{SnapshotFiles: snapshotFiles}, nil
}

```

```bash
sed -n '149,175p' cmd/ateom-gvisor/runsc.go | grep -v '^\s*// "-'
```

```output
func (r *runsc) cmdCheckpoint(ctx context.Context, containerName, checkpointPath string) error {
	slog.InfoContext(ctx, "About to run runsc checkpoint", slog.String("container", containerName))

	cmd := exec.CommandContext(
		ctx,
		r.path,
		"-log-format", "json",
		"--alsologtostderr",
		"-root", ateompath.RunSCStateDir(r.actorUID),
		"checkpoint",
		"-image-path", checkpointPath,
		containerName, // Name of the container
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err := reaper.RunCommand(cmd)
	if err != nil {
		return fmt.Errorf("while running `runsc checkpoint`: %w", err)
	}
	return nil
}

```

Back in atelet, the checkpoint type decides where the files go. EXTERNAL uploads them to object storage. LOCAL moves them into an on-node directory; pause uses this, as described below. atelet then unmounts external volumes and resets the actor's directories, which leaves the worker clean for the next actor:

```bash
sed -n '703,730p' cmd/atelet/main.go
```

```output
	switch req.GetType() {
	case ateletpb.CheckpointType_CHECKPOINT_TYPE_EXTERNAL:
		// TODO(#362): Because we do not cache the external snapshot files when upload fails, we have to mark the Actor as CRASHED.
		if err := s.uploadExternalCheckpoint(ctx, req, checkpointDir, sandboxRec); err != nil {
			dPersist = time.Since(tPersist)
			return nil, fmt.Errorf("while uploading external snapshot: %w", err)
		}
	case ateletpb.CheckpointType_CHECKPOINT_TYPE_LOCAL:
		if err := s.moveLocalCheckpoint(ctx, req, checkpointDir, sandboxRec); err != nil {
			dPersist = time.Since(tPersist)
			return nil, fmt.Errorf("while moving to local snapshot: %w", err)
		}
	default:
		return nil, fmt.Errorf("unexpected checkpoint type: %v", req.GetType())
	}
	dPersist = time.Since(tPersist)

	if err := s.unmountExternalVolumes(ctx, actorUID, req.GetSpec().GetVolumes()); err != nil {
		return nil, fmt.Errorf("while unmounting external volumes: %w", err)
	}

	// Note: we do not crash the actor if resetting the directory fails.
	if err := resetActorDirs(actorUID); err != nil {
		return nil, fmt.Errorf("while resetting actor dirs: %w", err)
	}

	return &ateletpb.CheckpointResponse{}, nil
}
```

The upload order is what makes a snapshot crash-safe. The files are zstd-compressed and uploaded in parallel, and the manifest goes last. Readers treat the manifest as the commit marker, so a crash mid-upload leaves orphaned objects but never a manifest pointing at missing files:

```bash
sed -n '795,829p' cmd/atelet/main.go
```

```output
// uploadSnapshot uploads rec's snapshot files from srcDir to uri (each
// zstd-compressed, concurrently), then the marshaled manifest. The manifest
// goes last, never in parallel: its presence is the commit marker — readers
// assume every file it lists is already present. A crash mid-upload thus
// leaves only orphaned files, never a manifest pointing at files that never
// landed; retries overwrite the deterministic object names.
func (s *AteomHerder) uploadSnapshot(ctx context.Context, uri resources.SnapshotURI, srcDir string, rec *sandboxAssetsRecord, templateAtespace, templateName string) error {
	g, gCtx := errgroup.WithContext(ctx)
	for _, fileName := range rec.SnapshotFiles {
		local := filepath.Join(srcDir, fileName)
		recordSnapshotSize(ctx, fileName, local, templateAtespace, templateName)
		g.Go(func() error {
			objectURI, err := uri.ObjectURI(fileName + ".zstd")
			if err != nil {
				return fmt.Errorf("while addressing %s in GCS: %w", fileName, err)
			}
			if err := ategcs.SendLocalFileToGCSWithZstd(gCtx, s.gcsClient, objectURI, local); err != nil {
				return fmt.Errorf("while uploading %s to GCS: %w", fileName, err)
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return err
	}

	manifest, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("while marshaling snapshot manifest: %w", err)
	}
	manifestURI, err := uri.ObjectURI(sandboxManifestName)
	if err != nil {
		return fmt.Errorf("while addressing snapshot manifest in GCS: %w", err)
	}
	if err := ategcs.SendBytesToGCS(ctx, s.gcsClient, manifestURI, manifest); err != nil {
```

Finally, `ensureSuspendedFinalized` does three things. It releases the worker; the outbox then tells every replica's `workercache` that the worker is free. It commits SUSPENDED with the new snapshot as the actor's external snapshot. Then it releases the snapshot that the new one replaced:

```bash
sed -n '348,446p' cmd/ateapi/internal/controlapi/workflow_suspend.go | grep -n -E '^\s*// [0-9]\.|Release|ACTOR_STATE_SUSPENDED'
```

```output
10:	var dGetActor, dReleaseWorker, dRefetchActor, dReleaseSnapshot, dUpdateActor time.Duration
16:			slog.Duration("release_worker", dReleaseWorker),
18:			slog.Duration("release_snapshot", dReleaseSnapshot),
29:	// 1. Free the worker (if it hasn't been freed yet)
33:		dReleaseWorker = time.Since(t)
47:	// 2. Finalize the actor: record its new external snapshot and mark it SUSPENDED. This
60:	// 3. Release the external snapshot this suspend replaces (latestActor.externalSnapshot)
64:	dReleaseSnapshot = time.Since(t)
69:	// 4. Commit the actor.
72:		toUpdate.Status.State = ateapipb.ActorState_ACTOR_STATE_SUSPENDED
```

**Pause** (`workflow_pause.go`) follows the same outline but sends a LOCAL checkpoint. The snapshot stays on the node's disk, so resuming on the same node skips the download entirely. That is the `Restore LOCAL` branch of `ensureAteletRestored` in section 6. Suspending a PAUSED actor doesn't need ateom, because its sandbox is already gone. `ensurePausedSnapshotUploaded` has atelet copy the local checkpoint to object storage with the same `uploadSnapshot`.

**Delete** (`workflow_delete.go`) marks DELETING, has atelet terminate the workload if the actor is running, then releases the worker and snapshots and removes the record.

Suspends can also start from below. A worker can ask for its actor to be suspended: `internal/ateomsuspend` sends the request from ateom to atelet over the support socket, and atelet forwards it to ateapi's `WorkerService`. At each hop, identity comes from the mTLS certificate, never from the request body:

```bash
sed -n '131,150p' cmd/atelet/ateomsupport.go
```

```output
func (s *ateomSupportServer) RequestActorSuspend(ctx context.Context, req *ateletpb.RequestActorSuspendRequest) (*ateletpb.RequestActorSuspendResponse, error) {
	// Identity comes only from the mTLS certificate, never from the request: a
	// worker can speak for the actors it hosts and no others. Which those are
	// is the control plane's to know, so it is checked there against the
	// worker this names.
	workerIdentity, err := authenticatedWorkerIdentity(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := s.workers.RequestActorSuspend(ctx, &ateapipb.RequestActorSuspendRequest{
		// Workers are global-scoped and named by their pod UID.
		Worker: &ateapipb.ObjectRef{Name: workerIdentity.PodUID},
		Actor: &ateapipb.ObjectRef{
			Atespace: req.GetActorAtespace(),
			Name:     req.GetActorName(),
		},
		ActorUid: req.GetActorUid(),
	}); err != nil {
		return nil, err
	}
```

```bash
sed -n '32,86p' cmd/ateapi/internal/workerservice/suspend.go
```

```output
// RequestActorSuspend suspends an Actor on behalf of the Worker hosting it. The
// caller must be an atelet running on the Worker's node.
func (s *Server) RequestActorSuspend(ctx context.Context, req *ateapipb.RequestActorSuspendRequest) (*ateapipb.RequestActorSuspendResponse, error) {
	caller, err := ateletauth.Authenticate(ctx, s.ateletSPIFFEID)
	if err != nil {
		return nil, err
	}
	// TODO: replace the three checks below with the generated
	// Validate_RequestActorSuspendRequest, which already enforces all of them
	// from the tags on the message. It lives in controlapi today, because that
	// package holds the only +k8s:validation-gen marker, and this package must
	// not depend on the Control service. Once the marker moves to a neutral
	// package both can import, call the generated validator here and drop
	// validateActorRef with it.
	//
	// Workers are global-scoped, so the reference carries no atespace.
	if errs := resources.ValidateGlobalObjectRef(req.GetWorker(), field.NewPath("worker")); len(errs) > 0 {
		return nil, status.Errorf(codes.InvalidArgument, "invalid worker: %v", errs.ToAggregate())
	}
	if errs := validateActorRef(req.GetActor(), field.NewPath("actor")); len(errs) > 0 {
		return nil, status.Errorf(codes.InvalidArgument, "invalid actor: %v", errs.ToAggregate())
	}
	if req.GetActorUid() == "" {
		return nil, status.Error(codes.InvalidArgument, "actor_uid is required")
	}

	// A golden actor may be suspended, but only by the template controller:
	// the suspend is what commits its snapshot, and the controller runs it once
	// the warmup window it set has elapsed. A golden actor boots and waits, so
	// it looks idle from the moment it starts, and honoring its own request
	// would commit the snapshot before that window ends -- leaving every actor
	// restored from the template to start from a workload that never warmed up.
	if req.GetActor().GetAtespace() == resources.GoldenActorAtespace {
		return nil, status.Errorf(codes.FailedPrecondition, "actors in atespace %q are golden actors, which cannot request their own suspend", resources.GoldenActorAtespace)
	}

	workerName := req.GetWorker().GetName()
	actorRef := resources.ActorRefFromObjectRef(req.GetActor())

	// Use authoritative state to authorize the request, never the request
	// itself: the caller proves which node it is on, and nothing else.
	worker, err := s.store.GetWorker(ctx, workerName)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, status.Errorf(codes.NotFound, "Worker %s not found", workerName)
		}
		return nil, fmt.Errorf("while fetching worker %s: %w", workerName, err)
	}
	if worker.GetNodeName() != caller.NodeName {
		// Do not disclose Workers on other nodes.
		slog.WarnContext(ctx, "Refusing a suspend request for a worker on another node",
			slog.String("worker", workerName),
			slog.String("worker_node", worker.GetNodeName()),
			slog.String("caller_node", caller.NodeName),
			slog.String("caller_pod", caller.PodName))
```

The control plane then checks that the actor really is assigned to that worker, and runs the ordinary `SuspendActor` workflow. The work comes back down the same Checkpoint path shown above. Golden actors are refused because their suspend is what commits the template snapshot, and only the template reconciler may trigger it after the warmup window.

## 11. Who is allowed to talk to whom

Almost every hop so far has been mTLS, and the identities all come from X.509 certificates. There are three kinds.

**Pod identity.** Every Substrate pod (ateapi, atelet, ateom, atenet) gets its certificate from Kubernetes' native `PodCertificateRequest` API. `podcertcontroller` is the signer. kube-apiserver has already attested which pod, service account and node the request comes from. The signer spreads requests across replicas by rendezvous hashing and issues a leaf valid for at most 24 hours:

```bash
sed -n '113,160p' cmd/podcertcontroller/internal/podidentitysigner/podidentitysigner.go
```

```output
	lifetime := 24 * time.Hour
	requestedLifetime := time.Duration(*pcr.Spec.MaxExpirationSeconds) * time.Second
	if requestedLifetime < lifetime {
		lifetime = requestedLifetime
	}

	notBefore := time.Now().Add(-2 * time.Minute)
	notAfter := notBefore.Add(lifetime)
	beginRefreshAt := notAfter.Add(-30 * time.Minute)

	spiffeURI := &url.URL{
		Scheme: "spiffe",
		Host:   "cluster.local",
		Path:   path.Join("ns", pcr.ObjectMeta.Namespace, "sa", pcr.Spec.ServiceAccountName),
	}

	template := &x509.Certificate{
		// Some golang certificate handling code assumes that if the parent and
		// template Subject fields compare equal, we are doing a self-signing
		// operation [1].
		//
		// I'm not sure if this is correct, but for defense in depth include
		// some random content in the subject.
		//
		// [1] https://cs.opensource.google/go/go/+/refs/tags/go1.27.0:src/crypto/x509/x509.go;l=1871
		Subject: pkix.Name{
			CommonName: rand.Text(),
		},
		BasicConstraintsValid: true,
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		URIs:                  []*url.URL{spiffeURI},
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		// AuthorityKeyID is automatically set to the SubjectKeyID of the parent
		// certificate, as long as we are not self-signing a root.
	}

	// Fields are sourced from the PCR spec (attested by kube-apiserver) rather
	// than the Pod object, which lacks the ServiceAccount and Node UIDs.
	podIdentity := &substratex509.PodIdentity{
		Namespace:          pcr.ObjectMeta.Namespace,
		ServiceAccountName: pcr.Spec.ServiceAccountName,
		ServiceAccountUID:  string(pcr.Spec.ServiceAccountUID),
		PodName:            pcr.Spec.PodName,
		PodUID:             string(pcr.Spec.PodUID),
		NodeName:           string(pcr.Spec.NodeName),
		NodeUID:            string(pcr.Spec.NodeUID),
```

The URI SAN is a SPIFFE ID of the form `spiffe://cluster.local/ns/<ns>/sa/<sa>`. The router and atunnel check each other by this ID (section 9). A private X.509 extension, under Google's enterprise OID arc, carries the full pod identity. With it, atelet can tell which worker pod is calling and which node it is on, without trusting anything in the request:

```bash
sed -n '31,76p' internal/substratex509/substratex509.go
```

```output
var (
	// GoogleSubstratePEN is the ASN.1 Private Enterprise Number arc used to
	// name X.509 extensions that communicate Substrate-specific concepts.
	GoogleSubstratePEN = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 11129, 2, 12}

	// oidPodIdentity identifies the Kubernetes PodIdentity X.509 extension specifically in substrate.
	oidPodIdentity = makeSubstrateOID(1)
	// oidActorIdentity identifies the Substrate ActorIdentity X.509 extension specifically in substrate.
	oidActorIdentity = makeSubstrateOID(2)
)

func makeSubstrateOID(subIDs ...int) asn1.ObjectIdentifier {
	base := asn1.ObjectIdentifier{}
	base = append(base, GoogleSubstratePEN...)
	base = append(base, subIDs...)
	return base
}

// PodIdentity is the Kubernetes Pod Identity of a pod, as embedded in the
// oidPodIdentity extension of its certificate.
type PodIdentity struct {
	Namespace          string
	ServiceAccountName string
	ServiceAccountUID  string
	PodName            string
	PodUID             string
	NodeName           string
	NodeUID            string
}

func AddPodIdentityToCertificate(pod *PodIdentity, template *x509.Certificate) error {
	if err := validatePodIdentity(pod); err != nil {
		return fmt.Errorf("while validating PodIdentity input: %w", err)
	}
	podIdentityBytes, err := json.Marshal(pod)
	if err != nil {
		return fmt.Errorf("while json-marshaling PodIdentity extension: %w", err)
	}

	template.ExtraExtensions = append(template.ExtraExtensions, pkix.Extension{
		Id:    oidPodIdentity,
		Value: podIdentityBytes,
	})

	return nil
}
```

```bash
sed -n '61,94p' cmd/atelet/ateomsupport.go
```

```output

func authenticatedWorkerIdentity(ctx context.Context) (*substratex509.PodIdentity, error) {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "missing peer credentials")
	}
	tlsInfo, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok || len(tlsInfo.State.PeerCertificates) == 0 {
		return nil, status.Error(codes.Unauthenticated, "missing peer certificate")
	}
	identity, err := substratex509.PodIdentityFromCertificate(tlsInfo.State.PeerCertificates[0])
	if err != nil || identity == nil {
		return nil, status.Error(codes.PermissionDenied, "invalid worker identity")
	}
	return identity, nil
}

// verifyClientOnSameNode returns a TLS callback that accepts only worker Pods
// scheduled on the atelet's node incarnation.
func verifyClientOnSameNode(node *substratex509.PodIdentity) func(tls.ConnectionState) error {
	return func(state tls.ConnectionState) error {
		if len(state.PeerCertificates) == 0 {
			return fmt.Errorf("worker certificate is required")
		}
		identity, err := substratex509.PodIdentityFromCertificate(state.PeerCertificates[0])
		if err != nil {
			return fmt.Errorf("parse worker Pod identity: %w", err)
		}
		if identity == nil || identity.NodeName != node.NodeName || identity.NodeUID != node.NodeUID {
			return fmt.Errorf("worker is not on node %q (%s)", node.NodeName, node.NodeUID)
		}
		return nil
	}
}
```

**Actor identity.** An actor needs its own identity for egress, separate from the worker pod it happens to be on. atunnel, in the worker pod, generates a key and sends a CSR up the chain atunnel → atelet (support socket) → ateapi `WorkerService.MintAteomActorCertificate`. ateapi checks that the caller is an atelet and that the actor UID still matches, then signs a one-hour certificate with the actor-identity CA. That certificate carries the actor's SPIFFE ID:

```bash
sed -n '44,100p' cmd/ateapi/internal/workerservice/certificate.go
```

```output
	// TODO(identity): This check should be handled by OpenFGA.
	if _, err := ateletauth.Authenticate(ctx, s.ateletSPIFFEID); err != nil {
		return nil, err
	}

	// TODO(authz): Authorization layer needs to check whether the caller has
	// the mintActorCertificate permission/relation with this actor.    This
	// could be an atelet (via the relationship of the atelet running the
	// actor), or the egress gateway (via a cluster-level grant?)

	// Verify that this actor exists in the store. It doesn't need to be
	// running, since we may need to issue certificates during actor boot / resume.
	dbActor, err := s.store.GetActor(ctx, resources.ActorRefFromObjectRef(req.GetActor()))
	if errors.Is(err, store.ErrNotFound) {
		return nil, status.Error(codes.NotFound, "actor not found")
	} else if err != nil {
		return nil, fmt.Errorf("while retrieving actor: %w", err)
	}
	if dbActor.GetMetadata().GetUid() != req.GetActorUid() {
		return nil, status.Error(codes.Aborted, "conflict; actor has been deleted and recreated")
	}

	// Parse the CSR.
	csr, err := x509.ParseCertificateRequest(req.GetCertificateSigningRequest())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "Failed to parse CSR: %v", err)
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "Failed to verify CSR signature: %v", err)
	}

	template := &x509.Certificate{
		URIs: []*url.URL{
			{
				Scheme: "spiffe",
				// TODO(identity): Must be configurable per-install, so that each install can set it to a unique value.
				Host: "substrate-actor.local",
				Path: path.Join("ateom-for-actor", dbActor.GetMetadata().GetAtespace(), dbActor.GetMetadata().GetName()),
			},
		},
		NotBefore:             time.Now().Add(-5 * time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  false,
	}

	// Sign and return the actor cert.
	chain, err := s.actorIDCAPool.CreateCertificate(template, csr.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("while signing certificate: %w", err)
	}

	return &ateapipb.MintAteomActorCertificateResponse{
		ActorCertificates: chain,
	}, nil
```

The actor's outbound traffic, redirected by nftables (section 8), leaves atunnel as an HTTP CONNECT to atenet's egress gateway, and atunnel presents this certificate. The gateway verifies the certificate against the actor-identity roots and reads the actor from the SPIFFE ID. It then evaluates that actor's egress policy against the destination. The actor never touches the key, so nothing it writes can change who it appears to be:

```bash
sed -n '317,356p' cmd/atenet/internal/router/egress/egress.go
```

```output
func (h *Handler) verifyActorCertificate(chain []*x509.Certificate) (resources.ActorRef, error) {
	leaf := chain[0]
	intermediates := x509.NewCertPool()
	for _, cert := range chain[1:] {
		intermediates.AddCert(cert)
	}

	now := time.Now()
	if now.Before(leaf.NotBefore) || !now.Before(leaf.NotAfter) {
		return resources.ActorRef{}, fmt.Errorf("actor certificate is outside its validity period (%s..%s)",
			leaf.NotBefore.Format(time.RFC3339), leaf.NotAfter.Format(time.RFC3339))
	}
	// An actor certificate is an end-entity credential. Refusing IsCA here stops
	// a leaked or mis-issued CA certificate from being replayed as a leaf: chain
	// verification alone would happily accept one.
	if leaf.IsCA {
		return resources.ActorRef{}, fmt.Errorf("actor certificate is a CA certificate")
	}

	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:         h.actorIdentityRoots,
		Intermediates: intermediates,
		CurrentTime:   now,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err != nil {
		return resources.ActorRef{}, fmt.Errorf("actor certificate is not signed by the actor-identity CA: %w", err)
	}

	// Check that this is an ateom certificate --- the SPIFFE URI should be of
	// the form `spiffe://${trustdomain}/ateom-for-actor/${atespace}/${actor}`.
	if len(leaf.URIs) != 1 {
		return resources.ActorRef{}, fmt.Errorf("actor certificate has %d URI SANs, want 1", len(leaf.URIs))
	}
	ref, err := resources.ActorRefFromAteomForActorSPIFFEURL(leaf.URIs[0])
	if err != nil {
		return resources.ActorRef{}, fmt.Errorf("while parsing actor from SPIFFE ID: %w", err)
	}
	return ref, nil
}

```

There is a second minting RPC, `Control.MintActorCertificate`, which issues `spiffe://substrate-actor.local/actor/<atespace>/<name>`. It refuses bearer-token callers, because a proof-of-possession credential must not be bootstrapped from a bearer one.

**API callers.** Section 4 showed `apiauthn` on the ateapi server. Its chained authenticator takes the first URI SAN of an mTLS peer certificate if there is one. Otherwise it verifies a bearer JWT against the configured OIDC providers; this is how `kubectl-ate` authenticates:

```bash
sed -n '109,125p' cmd/ateapi/internal/apiauthn/apiauthn.go
```

```output
// chainedServerAuthenticator first checks mTLS peer, then checks
// a bearer token in the header.
type chainedServerAuthenticator struct {
	// jwt authenticates clients that did not present a certificate identity
	// (e.g. kubectl-ate, which dials without a client certificate).
	jwt jwtServerAuthenticator
}

func (a chainedServerAuthenticator) authenticate(ctx context.Context) (context.Context, error) {
	if id, ok := mtlsPeerIdentity(ctx); ok {
		return principal.InjectContext(ctx, principal.PrincipalInfo{
			ID:   id,
			Kind: principal.KindMTLS,
		}), nil
	}
	return a.jwt.authenticate(ctx)
}
```

Authentication is in place, but authorization mostly is not. The principal is put on the context, yet the Control handlers still carry `TODO(authz)` markers where permission checks will go (for example, "does this caller have mintActorCertificate on this actor?"). The checks that are enforced today are structural: atelet-only RPCs check the atelet SPIFFE ID, a worker can speak only for actors assigned to it, atunnel accepts only the router, and gVisor isolates the workload itself.

## 12. The operator's side: kubectl-ate, and a second sandbox

Humans drive all of this through `kubectl-ate`, a kubectl plugin built with cobra. Its verbs mirror the Control RPCs:

```bash
sed -n '23,37p' cmd/kubectl-ate/internal/cmd/verbs.go
```

```output
var (
	getCmd     = &cobra.Command{Use: "get", Short: "Display one or many resources"}
	createCmd  = &cobra.Command{Use: "create", Short: "Create a resource"}
	updateCmd  = &cobra.Command{Use: "update", Short: "Update a resource"}
	deleteCmd  = &cobra.Command{Use: "delete", Short: "Delete a resource"}
	pauseCmd   = &cobra.Command{Use: "pause", Short: "Pause a resource"}
	resumeCmd  = &cobra.Command{Use: "resume", Short: "Resume a resource"}
	suspendCmd = &cobra.Command{Use: "suspend", Short: "Suspend a resource"}
	logsCmd    = &cobra.Command{Use: "logs", Short: "Print the logs for a resource"}
	topCmd     = &cobra.Command{Use: "top", Short: "Display resource (CPU/Memory) usage"}
)

func init() {
	rootCmd.AddCommand(getCmd, createCmd, updateCmd, deleteCmd, pauseCmd, resumeCmd, suspendCmd, logsCmd, topCmd)
}
```

Each subcommand is a thin wrapper around one RPC. `kubectl ate resume actor foo` is the same `ResumeActor` call that atenet makes on the first request:

```bash
sed -n '206,228p' cmd/kubectl-ate/internal/cmd/actor.go
```

```output
var resumeActorCmd = &cobra.Command{
	Use:   "actor <actor-name>",
	Short: "Resume an actor",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		apiClient, err := ateclient.NewClient(ctx, kubeconfig, k8sContext, endpoint, tokenFile, traceEnabled)
		if err != nil {
			return fmt.Errorf("failed to connect to ate-api-server: %w", err)
		}
		defer apiClient.Close()

		actorRef := resources.ActorRef{Atespace: resumeActorAtespaceFlag, Name: args[0]}
		resp, err := apiClient.ResumeActor(ctx, &ateapipb.ResumeActorRequest{
			Actor: actorRef.ToObjectRef(),
		})
		if err != nil {
			return fmt.Errorf("failed to resume actor: %w", err)
		}

		return printer.PrintActorTo(cmd.OutOrStdout(), resp.GetActor(), outputFmt)
	},
}
```

`ateclient.NewClient` either dials a given `--endpoint` directly, or port-forwards to the ateapi Service using the kubeconfig. Either way, the client authenticates with a bearer token, which ateapi's JWT authenticator verifies (section 11):

```bash
sed -n '131,153p' internal/ateclient/builder.go
```

```output
func NewClient(ctx context.Context, kubeconfigPath, k8sContext, endpoint, tokenFile string, traceEnabled bool) (*Client, error) {
	tp, err := initTracing(ctx, traceEnabled)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize tracing: %w", err)
	}

	var cli *Client
	if endpoint != "" {
		cli, err = dialDirect(ctx, kubeconfigPath, k8sContext, endpoint, tokenFile, traceEnabled)
	} else {
		cli, err = dialPortForward(ctx, kubeconfigPath, k8sContext, tokenFile, traceEnabled)
	}

	if err != nil {
		if tp != nil {
			_ = tp.Shutdown(ctx)
		}
		return nil, err
	}

	cli.tracerProvider = tp
	return cli, nil
}
```

**ateom-microvm.** gVisor is not the only sandbox. `cmd/ateom-microvm` implements the same `Ateom` gRPC service using Cloud Hypervisor micro-VMs: `RunWorkload` in `run.go`, `CheckpointWorkload` in `checkpoint.go`, `RestoreWorkload` in `restore.go`. atelet and the control plane don't know which one a worker pool runs. The contract is the ateom proto. The interesting difference is memory. A VM restored "on demand" pages memory in lazily, so its next snapshot is only a delta, and ateom merges it back onto the restore source to keep every snapshot self-contained:

```bash
sed -n '239,264p' cmd/ateom-microvm/checkpoint.go
```

```output
	// Diff-snapshot completion for an OnDemand-restored actor: CH's snapshot here is
	// sparse — only the pages faulted in since the OnDemand restore — so on its own
	// it's INCOMPLETE (the un-faulted pages were being demand-paged from the restore
	// source). Overlay it onto that source to rebuild a COMPLETE memory-ranges, so the
	// snapshot is self-contained and re-restorable. (A cold-run actor has no restore
	// source and its snapshot is already complete — no merge.)
	if ra != nil && ra.snapshotIsSelfContained {
		// Eager restore already pulled every populated extent into guest memory, so
		// what cloud-hypervisor just wrote is the whole guest, not a delta. Merging
		// would copy the entire resident set onto the restore source for nothing.
		slog.InfoContext(ctx, "Snapshot is self-contained (eager restore); skipping merge",
			slog.String("id", actorUID))
	} else if ra != nil && ra.restoreSourceDir != "" {
		base := filepath.Join(ra.restoreSourceDir, "memory-ranges")
		delta := filepath.Join(checkpointDir, "memory-ranges")
		tMerge := time.Now()
		// Reuse base's on-disk working set (rename + overlay) instead of copying it —
		// CH is paused and about to be torn down, and base is discarded after. See
		// MergeDeltaIntoBase. (Falls back to the copying merge across filesystems.)
		if err := ch.MergeDeltaIntoBase(ctx, base, delta); err != nil {
			return 0, fmt.Errorf("while merging OnDemand delta into restore source: %w", err)
		}
		slog.InfoContext(ctx, "Merged OnDemand delta into base (complete snapshot)",
			slog.String("id", actorUID), slog.Duration("merge", time.Since(tMerge)))
	}

```

## 13. The whole trip, in one breath

1. An operator applies a **WorkerPool**. atecontroller turns it into a Deployment of worker pods, each running an **ateom** with atunnel. The ateapi syncer records each ready pod as a **Worker**, and the outbox fans this out to every replica's `workercache`.
2. `CreateActorTemplate` makes the template reconciler boot a **golden actor**, wait out the warmup, suspend it, and tag the snapshot. `CreateActor` then stores a SUSPENDED actor that borrows that snapshot.
3. The first HTTP request for the actor reaches **atenet**'s Envoy with `ate-target-actor`. The ext_proc resumer collapses concurrent requests into one `ResumeActor`.
4. **ateapi** takes the actor's lease, picks a free worker from the cache, binds it with a row-locked transaction, and calls **atelet** on that node.
5. **atelet** fetches the snapshot manifest, downloads the snapshot while it prepares assets and OCI bundles, and calls **ateom** over the pod's unix socket.
6. **ateom** runs `runsc restore` inside the sandbox, probes it awake, and wires the actor's netns (169.254.17.2) to atunnel. ateapi commits RUNNING.
7. Envoy routes the request by ORIGINAL_DST to the worker's atunnel over mTLS. atunnel checks the router's SPIFFE ID and that the actor is still active there, then proxies into the sandbox. Outbound traffic goes back out through atunnel to the egress gateway, carrying the actor's certificate.
8. Later, a suspend (from an operator, or requested by the worker itself) runs the path in reverse: checkpoint, upload with the manifest last, release the worker, commit SUSPENDED. The worker is warm again for the next actor.

Every step is an idempotent `ensureX` under a lease, and every state change is a versioned store update, so any process can crash at any point and a retry finishes the job.

