# How geese works

An application that runs as several processes on one PostgreSQL database has a problem every time it migrates. During a rolling deploy, old and new processes serve side by side. A migration run before the rollout breaks the old processes; one run after it breaks the new ones; one run by whichever process starts first races the others. The usual answer is to write every migration in expand-and-contract steps, by hand, across several releases.

geese takes the other way: the processes agree, in the database, on when it's safe to migrate, and stop for a moment while it happens.

## The flock

| | |
| --- | --- |
| **Goose** | One process. It `Join`s the flock and `Fly`s until it `Land`s. |
| **Honk** | Every two seconds a goose says, in `geese_flock`, the schema versions and capabilities its image serves, and the protocol it speaks. A goose not heard for ten seconds has left the flock. |
| **Grounded** | A goose that can't honk, or can't serve what the flock has, admits no work and cancels what it had, before the others count it gone. It flies again once a honk is heard. |
| **Leader** | Whoever holds the leader's advisory lock, and only in **calm weather** (a Kubernetes rollout has settled). It alone migrates, switches capabilities on and raises the oldest page served. When it lands, another takes over. |
| **Heading** | Where the flock is about to migrate to. Every goose hears it on its next honk and **gathers** for it: holds new work, interrupts its streams, and lets what's in flight finish. |
| **Flight plan** | The migration window, kept in the repository: at most four active migrations, and the schema versions an image serves. |

## A migration, step by step

1. A new image rolls out. Its geese join the flock and honk that they serve versions 6 and 7; the old image's geese honk 6.
2. The rollout finishes: every goose heard now serves 7, and the weather is calm.
3. The leader sets the heading to 7. On their next honk, every goose gathers: new requests wait, sockets close (their clients reconnect), and requests in flight finish, or are cancelled after ten seconds.
4. When every goose says it has gathered, the leader runs migration 7.
5. The leader clears the heading. Held requests run, on the new schema's code; nothing ran the old code against the new schema.

The whole stop is a few seconds. If any goose doesn't gather within thirty seconds, the leader lets the flock go on as it was and tries again later. If the leader dies mid-migration, the next one finds the heading and finishes.

A migration that fails leaves the schema as it was, as the flock migrates only in a transaction, so the flock goes on serving at the schema applied, and the leader logs why. It tries again after thirty seconds, then twice as long after each failure in a row, up to ten minutes; a release that fixes the migration migrates at the next try. Each try pauses the flock for a few seconds, so a migration must also be short: a backfill or a long index build runs in the background instead, with a [capability](capabilities.md) whose `Ready` says when it's done. `geese_flight` says what failed, how often, and when the next try is:

```sh
psql -c "SELECT failed_for, failures, retry_at, failure FROM geese_flight"
``` A goose that can't serve where the flock is heading stays grounded, so an old image started by mistake never runs against a schema it doesn't know.

## Capabilities and the oldest page served

The same agreement decides two more things:

- **When a feature switches on.** A capability activates for the whole deployment once its schema is in place and every goose serves it, so processes never disagree about what a client may use.
- **When a client must reload.** The flock serves pages from the oldest release major any goose runs, and says so; a page from an older major stops and loads the current release.

See [capabilities and the oldest page served](capabilities.md) for the server's half and the contract a client keeps.

## The flight plan

`flightplan.json`, beside the migrations, read by the image and by the `geese` command:

```json
{"maxActiveVersions": 4, "minimumSupportedVersion": 1, "latestMigrationVersion": 3, "minimumServeVersion": 2, "maximumServeVersion": 3}
```

| | |
| --- | --- |
| `maxActiveVersions` | How many migrations the repository may have before they're rolled into a new baseline: at most four. |
| `minimumSupportedVersion` | The first migration, the **baseline**. A database older than it is migrated by an older release first. |
| `latestMigrationVersion` | The last migration. |
| `minimumServeVersion`, `maximumServeVersion` | The schema versions this release serves, at most four. It serves the latest, and may serve the ones before it, for a rollout from a release that doesn't have the latest migration yet, and one after it, for a rollback from a release that does. Raising `minimumServeVersion` is raising the window's **floor**. |

```sh
go run github.com/jarensaa/geese/cmd/geese check            # the migration window, in CI
go run github.com/jarensaa/geese/cmd/geese check --staged   # in a pre-commit hook
go run github.com/jarensaa/geese/cmd/geese tokens --check   # the compile-time tokens are current
```

The command looks for `flightplan.json` at the repository's root, its migrations in `migrations/` beside it, and the tokens file in `support/generated.go` beside it. `--plan server/db/flightplan.json` moves all three; `--migrations`, `--source` and `--out` move one.

`geese tokens` writes a constant for each schema version the image serves (`ServeV2`) and each active migration (`MigrateV1`). Code kept for an older schema lives in files named `compat_v0002.go` that name its token (`const _ = support.ServeV2`); raising the window's floor removes the token, and the build fails until the old code is deleted. Before a fifth migration, the chain is rolled into a new baseline, and `goosetest.RollupKeepsSchema` proves, in CI, that the rollup builds exactly the schema main's migrations did.

## Rules of the flock

- **One flock to a database.** Its tables (`geese_flock`, `geese_flight`) live beside the application's, outside its migrations, and its advisory locks are database-wide.
- **PostgreSQL only.** It relies on advisory locks, `clock_timestamp()` and the catalog.
- **Every goose speaks one protocol.** A goose writes the protocol it speaks with every honk, and a leader decides nothing while it hears a goose of another protocol, or a pod of the coordinator geese came from. See [the protocol](../PROTOCOL.md).
- **Admit all database work.** Work that isn't admitted isn't waited for by a migration, and isn't cancelled when its goose is grounded.
- **Never migrate a database in use from outside the flock.** The flock migrates it. Offline migration is for recovery, with everything stopped.
- **A rollback image must serve the schema already applied.** Databases don't go backwards.
