# Decision 0006: The flock never pauses

Status: Accepted, and amended by [decision 0007](0007-downtime-when-asked-for.md): a migration may ask for downtime. Supersedes the pause of [decision 0001](0001-live-schema-migrations.md) (*Draining for an upgrade*) and [decision 0005](0005-pausing-only-when-it-must.md), whose classification, declarations and lock waits stand.

## Context

Decision 0001 paused the flock for every migration: each goose held new work, closed its streams, and let what was in flight finish, cancelling what ran past ten seconds. Decision 0005 kept the pause only for migrations that could break the code running, and found most migrations don't. Decision 0004 made every migration transactional, so one that fails changes nothing.

What was left of the pause was its cost. Requests waited, sockets reconnected, long requests failed, and all of it for migrations that, by then, could always be made another way: in steps that each keep the running code working, which geese had learned to check. Every one of those steps is something decisions 0004 and 0005 already let the flock run without a pause: add a column, fill it in the background behind a capability, switch the code, declare the old column unused, drop it.

## Decision

- **The flock serves throughout every migration.** Nothing is held, interrupted or cancelled for one. The leader runs a migration only when its migrator says it keeps the code already serving working (`Migrator.Change`), and every goose heard serves it and declares unused what it drops.
- **What the flock won't run, `geese check` fails.** A rename, a changed type, data reshaped: the check says why, and the change is made in steps that each keep the running code working. A leader that meets one anyway records it as a failed attempt, and changes nothing.
- **Indexes on tables in use are built concurrently.** `CREATE INDEX CONCURRENTLY` blocks no writes, but runs outside a transaction, so the flock runs it only alone, after `DROP INDEX CONCURRENTLY IF EXISTS` of the same index: a build that fails leaves an invalid index, which the next attempt drops. It isn't held to the lock timeout, as its waits hold up no one.
- **The heading is a fence, no more.** While the leader migrates, `geese_flight.heading_for` and `dropping` keep out a goose that joins meanwhile and couldn't serve the migration, or may still use what it drops. A transactional migration has no half-done state for a heading to recover.
- **Admission is one call.** `AdmitStream` and `ErrMigrating` go: a stream is work like any other, cancelled only when its goose loses its place.

## Consequences

- No migration disrupts the flock: no held requests, no closed sockets, no cancelled work, and no query waits behind a migration for more than a moment.
- A change the old code would notice takes several releases, as expand and contract always did; geese checks each step instead of trusting it, and runs the migrations among them when it's safe.
- Data migrations aren't migrations: a backfill is background work, admitted like any other, with a capability whose `Ready` says when it's done.
- The protocol loses `geese_flock.gathered_for` and `geese_flight.departed_at` and `pausing`, and gains `geese_flight.dropping`. Protocol 1 has no release yet, so it's changed in place.
- The library is two hundred lines shorter, and the tests of the pause are gone.
