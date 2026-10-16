# How geese works

An application that runs as several processes on one PostgreSQL database has a problem every time it migrates. During a rolling deploy, old and new processes serve side by side. A migration run before the rollout breaks the old processes; one run after it breaks the new ones; one run by whichever process starts first races the others. The usual answer is expand and contract: every change made in steps that each keep the running code working, over several releases, by hand, and trusted.

geese runs those steps for you, and checks them. The processes agree, in the database, on when the next migration may run; geese runs only migrations that keep the code already serving working, and drops a column only once every image running says its code no longer touches it. The flock serves throughout: nothing is held, cancelled or reconnected.

## The flock

| | |
| --- | --- |
| **Goose** | One process. It `Join`s the flock and `Fly`s until it `Land`s. |
| **Honk** | Every two seconds a goose says, in `geese_flock`, the schema versions and capabilities its image serves, what its code no longer touches, and the protocol it speaks. A goose not heard for ten seconds has left the flock. |
| **Grounded** | A goose that can't honk, or can't serve what the flock has, admits no work and cancels what it had, before the others count it gone. It flies again once a honk is heard. |
| **Leader** | Whoever holds the leader's advisory lock, and only in **calm weather** (a Kubernetes rollout has settled). It alone migrates, switches capabilities on and raises the oldest page served. When it lands, another takes over. |
| **Heading** | Where the flock is migrating to. While it's set, a goose that can't serve it, or may still use what the migration drops, can't join. |
| **Flight plan** | The migration window, kept in the repository: at most four active migrations, the schema versions an image serves, and what its code no longer touches. |

## A migration, step by step

1. A new image rolls out. Its geese join the flock and honk that they serve versions 6 and 7; the old image's geese honk 6.
2. The rollout finishes: every goose heard now serves 7, and the weather is calm.
3. The leader reads migration 7, and checks it keeps the code already serving working ([what the flock runs](#what-the-flock-runs)), and that every goose declares unused what it drops.
4. It sets the heading to 7, and runs the migration while every goose goes on serving.
5. It clears the heading. Each goose hears the new version on its next honk; code that branches on `flock.Version()` takes the new path.

If the leader dies mid-migration, the migration's transaction rolls back, and the next leader runs it again.

A migration that fails leaves the schema as it was, as the flock migrates only in a transaction, and the leader logs why. It tries again after thirty seconds, then twice as long after each failure in a row, up to ten minutes; a release that fixes the migration migrates at the next try. `geese_flight` says what failed, how often, and when the next try is:

```sh
psql -c "SELECT failed_for, failures, retry_at, failure FROM geese_flight"
```

## What the flock runs

geese reads each migration (package goose's `Classify`), and runs only what keeps the code already serving working, and holds its locks briefly:

| | |
| --- | --- |
| Making a table, and indexes on it | Nothing serving reads it yet. |
| Adding a column that's nullable, or has a constant default | Old queries don't name it, and old inserts get the default. |
| Adding a constraint `NOT VALID`, and validating it in a migration of its own | Neither checks every row while it holds up writes. |
| Adding a type, a sequence, a schema, an enum value or a comment | Nothing serving uses them. |
| Building an index on a table in use, `CONCURRENTLY` | It doesn't block writes. Goose runs it outside a transaction, so the migration does only this, after `DROP INDEX CONCURRENTLY IF EXISTS` of the same index: a build that fails is tried again afresh. |
| Dropping a table or a column every running image declares unused | [Below](#dropping-what-isnt-used). |

Anything else, the flock won't run: a rename, a changed type, an index built on a table in use without `CONCURRENTLY`, a column with a default that isn't a constant or a constraint that checks every row, changes to data, and anything geese can't read for certain. `geese check` fails it in CI, and says why:

```
flight plan: migrations/00003_rename.sql would disrupt the flock: geese can't tell code already serving keeps working through "ALTER TABLE lists RENAME name TO title": make it in steps that do
```

A migration still takes brief locks, as PostgreSQL alters a table only under one, and a query arriving while the lock is held waits the milliseconds it takes. But the migration waits at most fifty milliseconds for a lock, so queries never queue behind one it's still waiting for; it lets go when it doesn't get it, and tries again a moment later. A migration that can't get its locks fails like any other, and is tried again later.

### Changing what code relies on

A rename, a new type, a column made `NOT NULL`, data reshaped: none of them keeps the old code working in one step, so each is made in steps that do, over releases. Renaming `lists.name` to `title`:

1. **Expand.** A migration adds `title`, nullable.
2. **Write both, and fill.** The release writes both columns, and a background job, admitted like any other work, copies `name` into `title` for the rows that don't have it, until a [capability](capabilities.md) whose `Ready` says no row lacks it is on. The capability waits for every goose to serve it, so no goose that writes only `name` is left.
3. **Switch.** Code reads `title` once the capability is on; the next release reads and writes only `title`, and declares `lists.name` unused.
4. **Contract.** A later release drops `name`, once every image running declares it unused.

Each step is a migration geese runs, or code geese's capabilities and declarations check.

### Dropping what isn't used

Whether code still uses a column is a fact about the images running, not about the schema, so each image says it. The release that stops using `lists.archived` declares it in its flight plan:

```json
{"maxActiveVersions": 4, "minimumSupportedVersion": 1, "latestMigrationVersion": 2, "minimumServeVersion": 1, "maximumServeVersion": 2, "unused": ["lists.archived"]}
```

and passes it on as `Config.Unused`, so each of its geese says so with every honk. A later release drops the column, and keeps declaring it. The leader drops it once every goose it hears declares it unused; while a goose of an older image that doesn't is still flying, the drop waits. An image that forgets holds the drop back; it never breaks.

A declaration is a claim about an image's code, so the application's tests check it: `geesetest.DropUnused` drops what the flight plan declares from the test database, and the tests then run on the schema the drop will leave. A query that still reads the column, `SELECT *` included, fails there.

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
- **Admit all database work.** Work that isn't admitted isn't cancelled when its goose is grounded, so it may run on after the flock stops counting it.
- **Never migrate a database in use from outside the flock.** The flock migrates it. Offline migration is for recovery, with everything stopped.
- **A rollback image must serve the schema already applied.** Databases don't go backwards.
