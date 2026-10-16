# Decision 0008: Rolling back across migrations that ran live

Status: Accepted. Replaces the serving window's "one version after the latest, for a rollback" of [decision 0001](0001-live-schema-migrations.md).

## Context

A database doesn't go backwards: geese never runs a migration's Down, as a rollback is when the newer image, which has the Down, is trusted least. So a rollback runs an older image on the newer schema. Until now an image served only the versions its flight plan named, up to `maximumServeVersion`, at most one past its latest migration: rolling back more than one release, or from a release whose plan didn't reach that far, was refused.

Since [decision 0006](0006-never-pausing.md), the flock runs a migration live only if the code serving before it keeps working with it. That code is the older image. A rollback across migrations that ran live is safe by construction; only the window's bookkeeping refused it. A migration with downtime ([decision 0007](0007-downtime-when-asked-for.md)) is different: it may change anything, and the code before it needn't work after.

## Decision

- **The flock keeps its journey.** `geese_journey` has a row for every migration the flock ran: whether it took downtime, and what it dropped. The leader writes it as it clears the heading, so a successor finishing a leader's migration writes it too.
- **An image serves a newer schema** while every migration since its latest has a row, none took downtime, and it declares unused everything they dropped. It runs its own newest code: `Version` says its latest. A migration with no row, such as a baseline applied by `init`, counts as downtime.
- **Everything else that keeps a goose out still does:** a capability switched on that it doesn't serve, and an oldest page served above its release.
- **An image that can't serve the schema is refused** with `ErrSchemaUnserved`, and its pods don't start: the release before them goes on serving, and the way out is a release forward.
- **The window ends at the latest migration.** `maximumServeVersion` goes; the flight plan now refuses a key it doesn't know, so an old or misspelt one is caught.

## Consequences

- A service rolls back as far as it likes across migrations that ran live, with no downgrade and no planning in the flight plan.
- A migration with downtime closes the way back to the releases before it, and `geese check` says so in review.
- The protocol gains `geese_journey`, part of the unreleased protocol 1.
