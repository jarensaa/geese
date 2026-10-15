# Getting started

This guide runs a small service as a flock of two processes, then ships a release with a migration while they serve. It's [`examples/quickstart`](https://github.com/jarensaa/geese/tree/main/examples/quickstart) in the repository.

You need Go 1.26 or newer, and PostgreSQL: Docker will do.

## Install

In a new module:

```sh
go mod init quickstart
go get github.com/jarensaa/geese
```

## A service

Three files make the service: its code, its migrations, and its flight plan.

`main.go`:

```go
package main

import (
	"cmp"
	"context"
	"embed"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/jarensaa/geese"
	"github.com/jarensaa/geese/geesehttp"
	"github.com/jarensaa/geese/goose"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

//go:embed flightplan.json
var flightplan []byte

func main() {
	ctx := context.Background()
	url := os.Getenv("DATABASE_URL")
	plan, err := geese.ParseFlightPlan(flightplan)
	if err != nil {
		panic(err)
	}
	migrations := goose.Migrations{FS: migrationFS, Dir: "migrations"}

	// "init" sets up an empty database, once, before the first process starts.
	if len(os.Args) > 1 && os.Args[1] == "init" {
		if err := goose.Migrate(ctx, url, migrations, plan, false); err != nil {
			panic(err)
		}
		return
	}

	// Join the flock: this process says what it serves, and stays in step with the others.
	flock, err := geese.Join(ctx, geese.Config{
		DatabaseURL: url,
		Name:        os.Getenv("HOSTNAME"),
		Release:     "1.0.0", // this build's version
		Window:      plan.Window(),
		Weather:     geese.ClearSkies{}, // kube.InCluster(...) under Kubernetes
		Migrator:    migrations,
	})
	if err != nil {
		panic(err)
	}
	defer flock.Land()

	// Fly until the process is told to stop.
	stopping, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	go flock.Fly(stopping)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /hello", hello)
	mux.Handle("GET /api/capabilities", geesehttp.Capabilities(flock))

	// Every request is admitted to the flock, so a migration waits for it.
	server := &http.Server{Addr: cmp.Or(os.Getenv("ADDR"), ":8080"), Handler: geesehttp.Guard(flock, mux)}
	go func() {
		if err := server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			panic(err)
		}
	}()

	// When told to stop, finish the requests in flight, then land.
	<-stopping.Done()
	_ = server.Shutdown(ctx)
}

func hello(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("hello\n")) }
```

`migrations/00001_baseline.sql`, the schema it starts from, as a [pressly/goose](https://github.com/pressly/goose) migration:

```sql
-- +goose Up
CREATE TABLE greetings (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    said text NOT NULL
);

-- +goose Down
DROP TABLE greetings;
```

`flightplan.json`, the migration window: this release serves schema version 1, and 1 is the latest migration.

```json
{"maxActiveVersions": 4, "minimumSupportedVersion": 1, "latestMigrationVersion": 1, "minimumServeVersion": 1, "maximumServeVersion": 1}
```

What `main` does, in order:

1. **Join.** The process says, in the database, which schema versions it serves (the flight plan's window), and what release it is.
2. **Fly.** Every two seconds it honks: it renews its place in the flock, and hears what the others are doing. When it leads, it migrates.
3. **Admit.** `geesehttp.Guard` admits every request to the flock, so a migration waits for requests in flight and holds new ones for the few seconds it runs.
4. **Land.** When told to stop, it stops flying, finishes its requests in flight, and leaves the flock, so the others stop waiting for it at once.

## Run it

Start PostgreSQL, and set up the empty database once:

```sh
docker run -d --name geese-db -e POSTGRES_PASSWORD=postgres -p 5432:5432 postgres:17-alpine
export DATABASE_URL="postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable"
go run . init
```

`init` refuses a database that's in use: from here on, only the flock migrates it. Now start two processes, as a deployment would:

```sh
go build -o quickstart .
HOSTNAME=one ./quickstart &
HOSTNAME=two ADDR=:8081 ./quickstart &
curl localhost:8080/hello
```

Both are in the flock:

```sh
docker exec geese-db psql -U postgres -c "SELECT name, serves_from, serves_to FROM geese_flock"
```

## Ship a migration

The next release adds a column. Its code works on schema 1 and on schema 2, so old and new processes can serve side by side during the rollout.

`migrations/00002_language.sql`:

```sql
-- +goose Up
ALTER TABLE greetings ADD COLUMN language text NOT NULL DEFAULT 'en';

-- +goose Down
ALTER TABLE greetings DROP COLUMN language;
```

`flightplan.json` now has 2 as its latest migration, and the release serves 1 to 2:

```json
{"maxActiveVersions": 4, "minimumSupportedVersion": 1, "latestMigrationVersion": 2, "minimumServeVersion": 1, "maximumServeVersion": 2}
```

Code that differs between the two branches on `flock.Version()`. Set `Release` to `"1.1.0"`, build, and roll out one process at a time, the way Kubernetes would:

```sh
go build -o quickstart .
kill %1; HOSTNAME=three ./quickstart &
kill %2; HOSTNAME=four ADDR=:8081 ./quickstart &
```

While a process of the old release is in the flock, nothing migrates: it can't serve schema 2. Once the last one has landed, the leader logs:

```
INFO geese: gathering for a migration target=2
INFO geese: migrated version=2
```

Every process held new requests and finished those in flight, the leader migrated, and the held requests ran on the new schema's code. A later release raises `minimumServeVersion` to 2, and deletes the code that kept schema 1 working; [`geese check`](concepts.md#the-flight-plan) in CI says when that's due.

## Next

- [How geese works](concepts.md): the flock, and a migration step by step.
- [Adopting geese](adopting.md): background work, Kubernetes, capabilities and CI, for a real service.
- [Transports](transports.md): gRPC, queues and jobs.
