# Adopting geese

## An application's checklist

1. **Migrations.** SQL migrations for pressly/goose, embedded in the image (`//go:embed migrations/*.sql`), numbered from `00001_baseline.sql`. Each keeps the code already serving working, as the flock runs it while it serves: it runs only [what it can tell does](concepts.md#what-the-flock-runs), and a change that doesn't is made [in steps that do](concepts.md#changing-what-code-relies-on), or in one migration that [asks for downtime](concepts.md#migrations-with-downtime). Each runs in a transaction (goose's default), so one that fails leaves the schema as it was; outside one, the flock only builds an index concurrently.
2. **The flight plan.** `flightplan.json` beside the migrations ([the flight plan](concepts.md#the-flight-plan)), read by the image (`geese.ParseFlightPlan`, embedded) and by `geese check`. The image passes its window as `Config.Window` and what it declares unused as `Config.Unused` ([dropping what isn't used](concepts.md#dropping-what-isnt-used)).
3. **Join and fly.** In `main`: `geese.Join`, `defer flock.Land()`, `go flock.Fly(ctx)` with a context that ends when the process is told to stop (`signal.NotifyContext`), and every request admitted: `geesehttp.Guard` around an HTTP handler, interceptors for gRPC ([transports](transports.md)). The process's health check is `flock.Ready()`. When told to stop, it finishes its work in flight (`server.Shutdown`), then lands: a goose that has stopped flying keeps serving for a few seconds, and is grounded after.
4. **Admit all work.** Background work (a sweep, a consumer, a backfill) admits itself with `flock.Admit` before it touches the database, and releases after. A goose that loses its place cancels it.
5. **Weather.** Under Kubernetes, `kube.InCluster(namespace, deployment)`, with a Role granting `get` on that Deployment and the namespace from the downward API. For a single process, `geese.ClearSkies{}`.
6. **A fresh database.** `goose.Migrate(ctx, url, migrations, plan, false)` once, before the first goose joins: a command of the image, never a step of a rollout. It migrates only an empty database, and leaves a current one alone.
7. **Capabilities.** An endpoint answering `flock.Capabilities` (over HTTP, `geesehttp.Capabilities(flock)` at `GET /api/capabilities`), and `flock.Require` in front of every feature that waits for one. Clients keep [the contract](capabilities.md).
8. **CI.** `geese check` (the window, every migration one the flock runs, and every drop declared unused) and `geese tokens --check` on every change; `geese check --staged` in a pre-commit hook; the integration tests on `geesetest`, on a database without what the flight plan declares unused (`geesetest.DropUnused` in the template that migrates it); and, when a pull request deletes, edits or renames a migration main has, `goosetest.RollupKeepsSchema` against main's migrations from the merge base.
9. **Releases.** The image's version is its SemVer release, set at build time and passed as `Config.Release`, and a release that breaks the API is a major one: that's what tells older pages to reload.

## Moving from the coordinator geese came from

geese is the schema coordinator of `go-monolith-template` (`server/schema`, `server/schemahttp`), extracted. Its tables are new (`geese_flock`, `geese_flight` for `schema_instances`, `schema_upgrade_state`), so an application's first release on geese is a rollout between two coordinators that don't read each other's tables. geese makes that safe from its side:

- The first goose to fly in the database **carries over** what the old coordinator decided: its active capabilities and the oldest page it served. Nothing that was on goes off.
- A goose **won't join** while the old coordinator is migrating (`schema_upgrade_state.target_version` set): it fails to start, and Kubernetes restarts it until the migration is done.
- A geese leader **decides nothing** while a pod of the old coordinator is heard (`schema_instances`): no migration, activation or raised floor until the old pods have landed.

The old coordinator can't see the geese, so the release that moves must leave it nothing to decide:

1. **Release the move on its own**: no new migration, and no new capability, in the release that switches to geese. The old leader then has nothing to migrate or activate while both run. Migrations and capabilities resume in the next release, once only geese fly.
2. **Keep the window**: the flight plan's numbers are the old migration policy's (`server/db/migration-policy.json` becomes `flightplan.json` with the same fields), and the tokens file keeps its package, so compatibility files don't change.
3. **Replace the code**: `server/schema` and `server/schemahttp` go; `db.NewLiveSchema` becomes `geese.Join` with `goose.Migrations`; `schema.Admit` becomes `flock.Admit`; `BootID` becomes `ID`; `Run` becomes `Fly`; `Close` becomes `Land`. Presence or other soft state keyed by the old boot id, and checked against `schema_instances`, is checked against `geese_flock` instead.
4. **Replace the scripts**: `scripts/check-migrations.mjs` and `scripts/generate-schema-support.mjs` become `geese check --plan server/db/flightplan.json` and `geese tokens --plan server/db/flightplan.json`; the rollup test becomes `goosetest.RollupKeepsSchema`.
5. **Clean up later**: once no release before the move can be rolled back to, a migration may drop `schema_instances` and `schema_upgrade_state`.
