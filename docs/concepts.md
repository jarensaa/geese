# How geese works

An application that runs as several processes on one PostgreSQL database has a problem every time it migrates. During a rolling deploy, old and new processes serve side by side. A migration run before the rollout breaks the old processes; one run after it breaks the new ones; one run by whichever process starts first races the others. The usual answer is to write every migration in expand-and-contract steps, by hand, across several releases.

geese takes the other way: the processes agree, in the database, on when it's safe to migrate, and, when a migration could break the code they're running, stop for a moment while it happens.

## The flock

| | |
| --- | --- |
| **Goose** | One process. It `Join`s the flock and `Fly`s until it `Land`s. |
| **Honk** | Every two seconds a goose says, in `geese_flock`, the schema versions and capabilities its image serves, and the protocol it speaks. A goose not heard for ten seconds has left the flock. |
| **Grounded** | A goose that can't honk, or can't serve what the flock has, admits no work and cancels what it had, before the others count it gone. It flies again once a honk is heard. |
| **Leader** | Whoever holds the leader's advisory lock, and only in **calm weather** (a Kubernetes rollout has settled). It alone migrates, switches capabilities on and raises the oldest page served. When it lands, another takes over. |
| **Heading** | Where the flock is about to migrate to. Every goose hears it on its next honk, and, when the migration needs a pause, **gathers** for it: holds new work, interrupts its streams, and lets what's in flight finish. |
| **Flight plan** | The migration window, kept in the repository: at most four active migrations, and the schema versions an image serves. |

## A migration, step by step

A migration that needs a pause, such as a rename:


1. A new image rolls out. Its geese join the flock and honk that they serve versions 6 and 7; the old image's geese honk 6.
2. The rollout finishes: every goose heard now serves 7, and the weather is calm.
3. The leader sets the heading to 7. On their next honk, every goose gathers: new requests wait, sockets close (their clients reconnect), and requests in flight finish, or are cancelled after ten seconds.
4. When every goose says it has gathered, the leader runs migration 7.
5. The leader clears the heading. Held requests run, on the new schema's code; nothing ran the old code against the new schema.

The whole stop is a few seconds. If any goose doesn't gather within thirty seconds, the leader lets the flock go on as it was and tries again later. If the leader dies mid-migration, the next one finds the heading and finishes.

A migration that fails leaves the schema as it was, as the flock migrates only in a transaction, so the flock goes on serving at the schema applied, and the leader logs why. It tries again after thirty seconds, then twice as long after each failure in a row, up to ten minutes; a release that fixes the migration migrates at the next try. Each try pauses the flock for a few seconds, so a migration must also be short: a backfill or a long index build runs in the background instead, with a [capability](capabilities.md) whose `Ready` says when it's done. `geese_flight` says what failed, how often, and when the next try is:

```sh
psql -c "SELECT failed_for, failures, retry_at, failure FROM geese_flight"
```

## Which migrations pause

Most migrations don't need the flock to stop: the code already serving keeps working while they apply. geese reads each migration (package goose's `Classify`), and lets the flock go on serving through:

| | |
| --- | --- |
| Making a table, and indexes on it | Nothing serving reads it yet. |
| Adding a column that's nullable, or has a constant default | Old queries don't name it, and old inserts get the default. |
| Adding a constraint `NOT VALID`, and validating it later | Neither checks every row while it holds up writes. |
| Adding a type, a sequence, a schema, an enum value or a comment | Nothing serving uses them. |
| Dropping a table or a column every running image declares unused | [Below](#dropping-what-isnt-used). |

Anything else pauses: a rename, an index on a table in use, a column with a default that isn't a constant or a constraint that checks every row, changes to data, and anything geese can't read for certain. A migration without a pause still takes brief locks: it waits at most a second for one, rather than hold up the queries queued behind it, and if it gives up, the next attempt pauses. Without a pause, the leader logs `migrating without a pause`; with one, it logs why.

`geese check` says which migrations will pause, and why:

```
Flight plan OK: 3 of 4 active migrations, serving 2–3.
  00002_colour.sql needs no pause
  00003_archive.sql needs no pause, once every image running declares lists.archived unused
```

### Dropping what isn't used

Whether code still uses a column is a fact about the images running, not about the schema, so each image says it. The release that stops using `lists.archived` declares it in its flight plan:

```json
{"maxActiveVersions": 4, "minimumSupportedVersion": 1, "latestMigrationVersion": 2, "minimumServeVersion": 1, "maximumServeVersion": 2, "unused": ["lists.archived"]}
```

and passes it on as `Config.Unused`, so each of its geese says so with every honk. A later release drops the column, and keeps declaring it. The leader drops it without a pause once every goose it hears declares it unused; while a goose of an older image that doesn't is still flying, the drop pauses, as any other migration does. An image that forgets costs a pause, never an outage.

A declaration is a claim about an image's code, so the application's tests check it: `geesetest.DropUnused` drops what the flight plan declares from the test database, and the tests then run on the schema the drop will leave. A query that still reads the column, `SELECT *` included, fails there. A goose that can't serve where the flock is heading stays grounded, so an old image started by mistake never runs against a schema it doesn't know.

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
| `unused` | The tables and columns this release's code no longer touches, as `table` or `table.column`: [dropping what isn't used](#dropping-what-isnt-used). |
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
