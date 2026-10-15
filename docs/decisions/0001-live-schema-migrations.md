# Decision 0001: Live schema upgrades and bounded compatibility windows

> **In geese's words.** This decision was made while the coordinator lived inside an application, and is kept as it was argued. Its pods are geese; a pod's lease is a goose's place in the flock, kept by honking (`geese_flock`, ex `schema_instances`); the upgrade fence is the flock's heading (`geese_flight`, ex `schema_upgrade_state`); draining is gathering; the deployment gate is the weather; the migration policy is the flight plan. Other ADR numbers are the application's own decisions.

Status: Accepted. What a failed migration does is amended by [decision 0004](0004-a-failed-migration.md).

## Context

All server pods use one PostgreSQL schema. A migration changes the schema seen by pods that are already running; a startup-only version check cannot protect them. Running migrations before a rolling application deployment can therefore break old pods immediately. A separate bridge-image release for every schema change would make deployment and rollback cumbersome. We want a normal application release to become ready on the current schema, then upgrade it when all active database clients can use the next schema.

We also need bounded migration and compatibility code. A database-backed registry cannot itself prove that an old source file is no longer needed, nor can a compiler recognize arbitrary legacy logic left in an ordinary package.

## Decision

### Schema capability and pod admission

- Each server image declares a contiguous range of schema versions it can serve, with a width of at most four versions. This is a tested capability claim, not merely a startup preference. A release containing migration `N+1` can serve both `N` and `N+1` during its ordinary rolling deployment; no separate bridge image is required.
- A database table records each process instance's unique boot ID, supported minimum and maximum schema versions, and a short lease expiry maintained using database time. The process becomes ready only after atomically registering and verifying that it supports the current schema. It must stop accepting requests, cancel or drain in-flight database work, and lose readiness before it can serve beyond an unrenewed lease. A restarted pod is a new instance, not a continuation of a stale row.
- Goose's version ledger remains authoritative for the applied schema version. A singleton coordination record tracks an in-progress target and must be reconciled with that ledger after a crash. Admission, registration, and the transition to an upgrade state use the same database coordination lock. During an upgrade, a new instance must support both the current and target versions. This fences out a newly starting incompatible pod after the leader's compatibility check. Heartbeats recheck the schema state; an already-running pod must fail closed if it becomes incompatible.
- Every workload that can access the application database must participate in this protocol, not just HTTP server pods. Before the first automatic upgrade, all pre-protocol pods must be drained by an explicit deployment gate: they have no registry rows and cannot safely be treated as absent.

### Automatic upgrade

- A pod may attempt leadership using a PostgreSQL advisory lock. We do not store a persistent `is_leader` boolean; leadership ends with the lock/session. Only the leader may begin a migration, and only one migration version advances at a time.
- Under the coordination lock, the leader verifies the current version, requires every unexpired instance lease to include the target version, and records an upgrade fence before doing schema work. It also requires sufficient ready compatible capacity and a settled deployment generation; absence of an old lease alone does not prove that a controller will not recreate an old pod. A mere `updated_at` comparison without fail-closed pod behavior and fenced admission is insufficient.
- The leader applies the versioned migration and verifies the resulting schema version before clearing the fence. If it dies or a migration fails, the fence remains; a successor must inspect the recorded state and actual Goose version and resume or require intervention. It must never assume that a failed migration is safe to reverse. Live schema steps must be bounded and transactional where PostgreSQL permits; large data backfills are separate, resumable operations.
- The application must work on every version it declares. New behavior that requires `N+1` stays disabled until the database reports `N+1`. Pods drain before a migration applies (see *Draining for an upgrade*), so no request runs `N`'s code against `N+1`.

### Draining for an upgrade

A migration may drop tables and columns the previous schema's code reads. A pod serving both schemas switches code paths when it learns the new version, on its next lease renewal, up to two seconds after the migration committed. Without draining, requests already running on the old code, or started before the pod noticed, would fail against the new schema. Expand and contract can't avoid that for every change, such as one that rewrites rows the old code reads in the same table.

- Once the leader raises the upgrade fence, each pod, at its next renewal, holds new work, interrupts streams (sockets, admitted with `AdmitStream`, whose pages reconnect), and waits for admitted requests to finish. Work still running after ten seconds is cancelled. Then the pod records in its lease that it has drained for the target. While a fence is pending, pods renew every quarter second, not every two.
- The leader migrates only once every live lease has drained for the target. A pod that hasn't within 30 seconds makes the leader lower the fence, so pods serve again, and try later.
- A pod stores the new version before it lets held work go, so held requests run on the new schema's code. Work held longer than a minute is refused with 503.
- Every upgrade drains, additive or not: correctness doesn't depend on someone judging a migration safe to apply under load. The cost is a pause of a few seconds per upgrade, once per release at most.

Expand → backfill → switch → contract is still how a change keeps rollback open: a contract migration still waits until no image that needs the old shape may be rolled back to.

### API and frontend contract during an upgrade

- Database schema versions are private server state. The frontend must not read a schema-version header, poll migration progress, or branch on migration numbers. Existing API operations must retain their meaning throughout the declared serving range; an implementation may use different SQL for each schema without changing the public contract.
- A new operation or request field that cannot be honored on the old schema stays inactive until the cluster-wide migration gate has cleared. [decision 0002](0002-capabilities.md) defines the client-facing capability endpoint and cluster-wide activation rule. The earlier optional field on the session response was replaced; the frontend learns whether a product feature is available, never the schema version.
- A valid call that races feature activation receives a documented JSON `503` with code `feature_unavailable`. It is not a `400` validation failure, and the server must not silently discard fields. New read operations must not invent temporary identifiers that change when the migration finishes. Once a capability is advertised, every pod eligible to receive traffic must be able to honor it.
- The already-deployed, pre-protocol image cannot be retrofitted with capability discovery. During the first mixed-image rollout, direct API callers must treat an absent capability as unavailable and avoid the new fields and routes; the new frontend does so. The new image never advertises the capability until the old pods have drained. Future releases must preserve this additive API rule before introducing new public operations.

### Rollback

- Before the schema advances, an application rollback may use any image compatible with the old schema. After it advances, rollback is limited to an image whose declared range includes the new schema. The registry does not make an incompatible old image safe.
- Production does not automatically run Goose down migrations. If no known-good image supports the new schema, recovery is a corrective roll-forward or a deliberate database restore with its operational and data-loss implications. Destructive contract migrations wait until the relevant rollback window has closed.

### Retirement of old code

- Serving compatibility and upgradeability have separate floors: a server need not serve an old schema merely because the migrator can still upgrade a database from it. The serving range may span at most four versions. The active migration set may contain at most four versions, including a baseline.
- Version-specific serving code and Go migration code must live in version-labelled files or packages; ordinary application code may dispatch at a boundary but must not contain the legacy implementation. Each version-labelled Go unit must reference a compile-time support token for its version and purpose (`serve` or `migrate`). Those tokens are generated from the checked-in schema policy before the supported build. Raising the applicable floor removes the token; `go build ./...` must fail until the expired code is deleted. CI checks token generation and guards. The compiler cannot enforce deletion of arbitrary, unlabelled legacy logic, so this source-layout rule is mandatory.
- SQL migration files are enforced by the pre-commit and CI policy checker. Once every supported database has reached a new baseline and older rollback images are retired, raising the migration floor requires deleting earlier SQL and Go migrations from the active build, replacing them with a complete baseline, and testing both fresh installation and upgrade from the previous supported version. History remains available in Git and prior release images; no database is silently stamped past missing migrations.
- Raising either floor is a separate, deliberate change. The live instance registry is evidence about active pods, not proof that every long-lived database or rollback target can be abandoned. Deployment state and the rollback window must be checked before the policy change is accepted.

## Consequences and implementation status

- One normal release can roll out on schema `N`, then migrate to `N+1` after incompatible instances are gone. It must carry temporary dual-schema support, but no extra bridge-image release is required.
- The repository implements leases, fencing, leader recovery, the schema-2 compatibility path, and the compile-time retirement token. The first major migration (2 → 3) is exercised by an integration test with an incompatible old lease. The coordination tables are an additive, idempotent bootstrap independent of the Goose application schema ledger.
- Kubernetes reads the settled Deployment and all selected pod UIDs before migrating. This protects the first transition from old pods that have no lease. Operationally, do not create a pre-protocol pod concurrently with this check; no database protocol can fence an image that never participates in it. The Deployment must be frozen during the first schema upgrade. The Kubernetes recipe uses in-pod coordination, not a migration-before-rollout Job. Once no pre-protocol image could serve the schema any more, the pod check was dropped: the gate now reads only the settled Deployment, and live leases answer for the pods.
