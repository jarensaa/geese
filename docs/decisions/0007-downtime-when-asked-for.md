# Decision 0007: Downtime, when a migration asks for it

Status: Accepted. Amends [decision 0006](0006-never-pausing.md), whose flock never stopped serving.

## Context

Decision 0006 runs every migration while the flock serves, and makes a change the running code would notice in steps that each keep it working, over several releases. That keeps a service up through any change. It isn't always worth it: a small service can take a few seconds of downtime more easily than three releases for a rename, and some changes, such as a primary key's type on a large table, are hard to make in steps at all.

Decision 0001 met that need with a pause for every migration, and decision 0005 with a pause for every migration geese couldn't read. Both made the downtime geese's choice, found out about in production. The author knows best whether a change is worth the steps, so the choice is theirs, and should be seen in review.

## Decision

- **A migration asks for downtime in so many words**: `-- +geese downtime`. The migrator says so (`Change.Downtime`), and geese then doesn't read its statements: it may change anything, data included, as long as it runs in a transaction, so one that fails leaves the schema as it was.
- **The flock roosts for it.** The leader sets the heading with `roosting`. On its next honk, every goose stops serving: `Admit` refuses new work with `ErrRoosting` (`503` with `Retry-After` over HTTP), and work in flight has ten seconds to finish before it's cancelled. Each goose says it has roosted once nothing runs, and the leader migrates once every goose heard has, giving up after thirty seconds.
- **It refuses, rather than holds.** New requests are answered unavailable, which clients already retry, instead of queuing: the downtime is honest, and the library keeps no queue of held work.
- **A roosting goose stays ready.** `Ready` stays true: it's the flock that's down, not the process, and a health check that failed every pod at once would leave nothing to send traffic to when the flock comes back.
- **Heard quickly.** While a heading is set, geese honk every quarter second, so the downtime is what the work in flight needs, the migration's own time, and about a second more.
- **Seen in review.** `geese check` lists each migration that takes the flock down.

## Consequences

- A service chooses, migration by migration, between several releases without downtime and one with a few seconds of it.
- A migration with downtime needs no declarations of what's unused: nothing runs the code before it, and every goose serves the version after it.
- The protocol gains `geese_flock.roosted_for` and `geese_flight.roosting`, part of the unreleased protocol 1.
