# Decision 0004: A failed migration lets the flock go on

Status: Accepted. Amends [decision 0001](0001-live-schema-migrations.md), which kept the fence up after a failed migration.

## Context

Decision 0001 left the fence (the flock's heading) in place when a migration failed, for a successor to inspect: it must never assume a failed migration is safe to reverse. While the heading is up, every goose gathers, so new work is held for a minute and then refused. A successor found the heading, migrated again, failed again, and so on: one bad migration, such as one whose constraint the data violates, took the whole service down until someone shipped a fix.

That protects the schema from a migration that fails part of the way, at the cost of the service. Most failures aren't part of the way: PostgreSQL runs DDL in a transaction, and a migration that fails in one leaves the schema exactly as it was. The old code serves it as well as it did a minute before.

## Decision

- **The flock migrates only in a transaction.** The goose adapter refuses a migration marked `-- +goose NO TRANSACTION` before running anything, and `geese check` fails one in CI. The baseline is exempt: it's applied to an empty database, outside the flock. Statements that can't run in a transaction aren't needed while the flock is paused (`CREATE INDEX` will do instead of `CREATE INDEX CONCURRENTLY`), and long ones belong in the background, with a capability.
- **A failed attempt lets the flock go on.** When the migration fails, or the flock doesn't gather in time, the leader clears the heading if the schema didn't move. Every goose serves again at the schema applied.
- **The next attempt waits.** `geese_flight` records the version, the failures in a row, when the next attempt may start, and why the last one failed. No leader tries again before then: thirty seconds after the first failure, twice as long after each one after, up to ten minutes. The record is in the database, not the leader, as any goose may lead the next attempt.
- **Success forgets.** A migration that applies clears the record.

## Consequences

- A migration that keeps failing pauses the service for a few seconds every ten minutes, not for good, and its error is logged and in `geese_flight.failure`.
- A release that fixes the migration migrates at the next attempt, within ten minutes.
- A migration that genuinely needs to run outside a transaction can't be applied by the flock: it's applied offline, with everything stopped (`goose.Migrate` with `Offline`), or rewritten.
- The protocol gains four columns on `geese_flight`. Protocol 1 had no release yet, so they're part of it.
