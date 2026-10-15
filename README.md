# geese

Live schema migrations for an application that runs as several processes on one PostgreSQL database. The processes fly as a flock: each says what it can serve, one leads, and the flock migrates only when every goose can make the journey.

Migration tools like goose apply migrations; geese decides **when**. It runs inside the application, with no Job, operator or sidecar, and makes three decisions from one agreement in the database:

- **When to migrate.** One version at a time, once every running process serves the next version, the rollout has settled, and every process has gathered: held new work, interrupted its streams and finished what was in flight. Work that was held then runs on the new schema's code. So a migration may drop what the previous schema's code read, without a hand-written expand-and-contract.
- **When a feature switches on.** A capability activates for the whole deployment at once, when its schema is in place and every process serves it, so processes never disagree about what a client may use.
- **When a page must reload.** The flock says the oldest release whose pages it still serves, so a page kept by a service worker never calls an API that's gone.

## The flock

| | |
| --- | --- |
| **Goose** | One process. It `Join`s the flock and `Fly`s until it `Land`s. |
| **Honk** | Every two seconds a goose says, in `geese_flock`, the schema versions and capabilities its image serves, and the protocol it speaks. A goose not heard for ten seconds has left the flock. A goose that can't honk is **grounded**: it admits no work, and cancels what it had, before the others count it gone. |
| **Leader** | Whoever holds the leader's advisory lock, and only in **calm weather** (a Kubernetes rollout has settled). It alone migrates, switches capabilities on and raises the oldest page served. When it lands, another takes over. |
| **Heading** | Where the flock is about to migrate to. Every goose hears it on its next honk and **gathers** for it. |
| **Flight plan** | The migration window, kept in the repository: at most four active migrations, and the schema versions an image serves. The `geese` command holds the repository to it. |

## Use

```go
plan, _ := geese.ParseFlightPlan(flightplanJSON)
flock, err := geese.Join(ctx, geese.Config{
	DatabaseURL:  os.Getenv("DATABASE_URL"),
	Name:         os.Getenv("POD_UID"),
	Release:      version, // the image's SemVer release, "dev" for none
	Window:       plan.Window(),
	Weather:      rollout, // kube.InCluster(namespace, deployment), or geese.ClearSkies{}
	Migrator:     goose.Migrations{FS: embeddedMigrations, Dir: "migrations"},
	Capabilities: []geese.Capability{{Name: "comments", MinimumSchema: 7}},
})
if err != nil {
	return err
}
defer flock.Land()
go flock.Fly(ctx)

server := &http.Server{Handler: geesehttp.Guard(flock, app)}
```

- Every unit of database work enters through `flock.Admit` (or `AdmitStream` for a socket). `geesehttp.Guard` does it for every HTTP request, and answers 503 while the goose is grounded.
- Background work admits itself the same way: `ctx, release, err := flock.Admit(ctx)`.
- `flock.Version()` is the schema version this process serves now, for code that differs between versions. `flock.ActiveCapabilities(ctx)` and `flock.OldestPage(ctx)` are what an API's capabilities endpoint returns.
- `flock.Ready()` is the health check: false while grounded.
- To start a fresh database before the first goose joins: `goose.Migrate(ctx, url, migrations, plan, false)`. It refuses a database in use; the flock migrates that, live.

### Packages

| Package | |
| --- | --- |
| `geese` | The flock: `Join`, `Fly`, `Admit`, the flight plan. |
| `geese/geesehttp` | Admits HTTP requests. |
| `geese/kube` | A Kubernetes Deployment's rollout as the flock's weather. |
| `geese/goose` | pressly/goose's SQL migrations as the flock's migrator, and migrating a fresh database. |
| `geese/geesetest` | PostgreSQL in Docker for tests, a database per test. |
| `geese/goose/goosetest` | Proves a rollup of migrations into a baseline changes no schema. |
| `cmd/geese` | Holds a repository to its flight plan, and writes its compile-time tokens. |
| [`@jarensaa/geese`](js/) | A page's side: what the flock has switched on, and moving to the current release. |

### The flight plan, in CI

```sh
go run github.com/jarensaa/geese/cmd/geese check            # the migration window
go run github.com/jarensaa/geese/cmd/geese check --staged   # in a pre-commit hook
go run github.com/jarensaa/geese/cmd/geese tokens --check   # the compile-time tokens are current
```

`server/db/flightplan.json`:

```json
{"maxActiveVersions": 4, "minimumSupportedVersion": 1, "latestMigrationVersion": 3, "minimumServeVersion": 2, "maximumServeVersion": 3}
```

`geese tokens` writes a constant for each schema version the image serves (`ServeV2`) and each active migration (`MigrateV1`). Code kept for an older schema lives in files named `compat_v0002.go` that name its token (`const _ = support.ServeV2`); raising the window's floor removes the token, and the build fails until the old code is deleted. Before a fifth migration, the chain is rolled into a new baseline, and `goosetest.RollupKeepsSchema` proves, in CI, that the rollup builds exactly the schema main's migrations did.

## Rules of the flock

- **One flock to a database.** Its tables (`geese_flock`, `geese_flight`) live beside the application's, outside its migrations, and its advisory locks are database-wide.
- **PostgreSQL only.** It relies on advisory locks, `clock_timestamp()` and the catalog.
- **Every goose speaks one protocol.** A goose writes the protocol it speaks with every honk, and a leader decides nothing while it hears a goose of another protocol, or a pod of the coordinator geese came from. See [PROTOCOL.md](PROTOCOL.md).
- **Never migrate a database in use from outside the flock.** The flock migrates it. Offline migration is for recovery, with everything stopped.
- **A rollback image must serve the schema already applied.** Databases don't go backwards.

## Documents

- [PROTOCOL.md](PROTOCOL.md): the tables, what each goose may assume of the others, and how a protocol changes.
- [Adopting geese](docs/adopting.md): an application's checklist, and moving from the coordinator geese came from.
- Why it works the way it does: [live schema migrations](docs/decisions/0001-live-schema-migrations.md), [capabilities](docs/decisions/0002-capabilities.md), [the oldest page served](docs/decisions/0003-oldest-page-served.md).
