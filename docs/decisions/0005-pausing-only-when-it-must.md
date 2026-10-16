# Decision 0005: The flock pauses only when a migration could break the code running

Status: Accepted. Amends [decision 0001](0001-live-schema-migrations.md), whose flock drained for every upgrade.

## Context

Decision 0001 drained the flock for every migration, additive or not, so that correctness never rested on someone judging a migration safe to apply under load. The price is a pause of a few seconds per migration: new requests wait, sockets reconnect, and latency spikes once per release.

Most migrations don't need it. Adding a table, or a nullable column, leaves every query the old code runs working. What decision 0001 guarded against is a migration that changes what running code relies on: a rename, a changed type, a dropped column some image still reads. The first kind can be told from the second by reading the migration; the second can't always.

Dropping a column is the case in between. It's safe without a pause exactly when no image running reads it, and that's a fact about the images, not about the schema: a release built before its author stopped using the column may still be flying.

## Decision

- **The migrator says what a migration changes.** A `geese.Classifier` tells the leader why the flock must pause for a migration, if it must, and what it drops. Package goose reads each SQL migration's statements, and needs no pause only for those it knows keep running code working while holding locks briefly: making tables and indexes on them; adding types, sequences, schemas, enum values and comments; adding a nullable column, or one with a constant default; adding constraints `NOT VALID`, and validating them; and dropping tables and columns. Everything else, and anything it can't read for certain, pauses. Judgment isn't needed in the safe direction: a migration geese misreads pauses.
- **Each image declares what its code no longer touches.** The flight plan's `unused` names tables and columns, the image passes them as `Config.Unused`, and each goose writes them with every honk. A drop needs no pause only when every goose heard declares everything it drops. A goose of an image that doesn't, such as one built before the declaration, makes the drop pause.
- **Tests check the declaration.** `geesetest.DropUnused` drops the declared names from the test database, so the application's tests run on the schema the drop will leave. This checks what reading the code can't: generated queries, `SELECT *`, an ORM.
- **A migration without a pause won't queue for a lock.** It waits at most a second for one, as an `ALTER TABLE` waiting behind a long query holds up every query queued behind it. One that gives up fails like any other ([decision 0004](0004-a-failed-migration.md)), and the next attempt pauses.
- **The heading says whether the flock pauses for it.** `geese_flight.pausing` is set unless the leader found the migration needs no pause; geese gather only when it is. A goose that can't serve the heading is still grounded, pause or not.

## Consequences

- Most releases migrate without a pause, and `geese check` says, in review, which migrations will pause and why.
- Expand and contract is rewarded: a release that stops using a column and declares it lets the next release drop it without a pause. One that forgets costs a pause, never an outage.
- A declaration is only as good as the tests run against `DropUnused`. A declared column that code still reads, in a path no test covers, fails requests in flight when it's dropped, as it would without geese.
- The protocol gains `geese_flock.unused` and `geese_flight.pausing`. Protocol 1 has no release yet, so they're part of it.
