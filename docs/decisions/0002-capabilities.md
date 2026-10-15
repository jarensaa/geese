# Decision 0002: Cluster-wide capability advertisement

> **In geese's words.** This decision was made while the coordinator lived inside an application, and is kept as it was argued. Its pods are geese; a pod's lease is a goose's place in the flock, kept by honking (`geese_flock`, ex `schema_instances`); the upgrade fence is the flock's heading (`geese_flight`, ex `schema_upgrade_state`); draining is gathering; the deployment gate is the weather; the migration policy is the flight plan. Other ADR numbers are the application's own decisions.

Status: Accepted. Amended by [decision 0003](0003-oldest-page-served.md): the endpoint also says the oldest release whose page the deployment serves.

## Context

[decision 0001](0001-live-schema-migrations.md) keeps database schema versions private and requires new API behavior to remain inactive until a live upgrade is safe. A field in `/api/session` based on the schema version the responding pod sees is not a contract for announcing features across a cluster: a schema can be ready while an older pod still lacks the API behavior, and separate pods must not disagree about what the frontend may use.

The frontend needs to discover product behavior, not database versions, image versions, or migration progress. A capability is a deployment-wide promise about an API contract. It is not a user's permission to perform an action.

## Decision

### Public contract

- Add a stable, read-only `GET /api/capabilities` endpoint to the OpenAPI contract and generated client. It returns the set of currently active, stable capability identifiers, for example `{"capabilities":["workspaceNotes"]}`. An absent or unknown identifier is treated as inactive by the client. The endpoint does not expose schema or server versions. Initially, responses use `Cache-Control: no-store`; clients may revalidate on app startup, window focus, and after an unavailable-feature response.
- The endpoint is independent of authentication and user-specific authorization. A logged-out client and a logged-in client see the same deployment capabilities; normal authorization still applies to every protected operation. Capability names describe meaningful API behavior, not individual UI controls.
- The frontend may show or enable a feature only after observing its capability. It must still handle a documented JSON `503` with code `feature_unavailable` if an operation races activation or temporarily becomes unavailable. An unavailable endpoint is equivalent to an empty capability set during the first rollout from images that do not implement this endpoint. This fallback is for discovery only; it does not permit silently changing existing API semantics.
- The dedicated endpoint replaces `/api/session` as the source of deployment capabilities. This first change removes the session field rather than carrying a second, weaker source of truth. It is a breaking API change and requires a major semantic release.

### Activation and enforcement

- Every request-serving pod declares the capability identifiers its image can implement in its database lease/registration, alongside its supported schema range. A declaration means its code can honor the capability once the capability's prerequisites are satisfied; on an older schema it must keep the behavior inactive. The declaration alone does not activate the capability.
- A leader may activate a capability only after its prerequisites are complete: the required schema is applied, any required backfill or external dependency is ready, every live request-serving pod declares support, all database clients satisfy the schema-compatibility gate, and the Kubernetes deployment gate confirms that the selected workload has settled. Missing leases and pre-protocol pods are not evidence of support. Activation is persisted in shared coordination state under the same admission/upgrade lock used by decision 0001, not inferred independently by each HTTP pod.
- All pods serve the same persisted active set from `/api/capabilities`. They enforce that set in the corresponding REST handlers, so direct callers and the MCP adapter cannot bypass it. The endpoint must never return a capability that the responding pod cannot honor. When coordination state cannot be checked reliably, readiness and feature activation fail closed rather than advertising a guessed value.
- Once a capability is active, admission and lease renewal reject an image that does not support it. A routine rollback therefore uses an image that supports both the current schema and every active capability. Do not automatically deactivate an advertised capability because a pod disappears or a rollback is requested: clients may already rely on it. Retiring or changing a capability requires a separate compatible API transition and an explicit coordinated decision.

### Evolution

- Additive API changes may be deployed before activation, but their use remains gated until all eligible pods can serve them. Existing operations must continue to work for old and new clients throughout the rollout. Capability identifiers have stable semantics; do not reuse a name for a different contract. A substantial incompatible change gets a new identifier and a compatible migration path.
- The frontend knows only capability identifiers and the resulting API types. It never branches on schema versions or pod/image versions. Generated TypeScript types come from OpenAPI; the capability endpoint and gated operations are tested through mixed-version and activation scenarios.

## Consequences

This adds coordination metadata and an activation gate, but gives the frontend one cluster-wide answer independent of which pod handles a request. The coordination tables are expanded additively outside the Goose schema ledger. The leader activates a capability only after its schema is applied, all live leases claim support, and the Kubernetes deployment gate has settled. Once a capability is active, an image that doesn't know it is **not** a safe rollback target, even if it can serve the schema: it can't honor the active-capability admission rule. Freeze the Deployment during activation and roll forward to a compatible image if recovery is needed. decision 0001 continues to govern schema safety; this ADR governs when a product/API capability becomes visible.
