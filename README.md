<p align="center">
  <img src="docs/assets/banner.jpg" alt="geese: distributed database schema migrations in Go" width="820">
</p>

<p align="center">
  <a href="https://pkg.go.dev/github.com/jarensaa/geese"><img src="https://pkg.go.dev/badge/github.com/jarensaa/geese.svg" alt="Go Reference"></a>
  <a href="https://github.com/jarensaa/geese/actions/workflows/ci.yml"><img src="https://github.com/jarensaa/geese/actions/workflows/ci.yml/badge.svg?branch=main" alt="CI"></a>
  <a href="https://github.com/jarensaa/geese/blob/main/LICENSE"><img src="https://img.shields.io/github/license/jarensaa/geese" alt="License: Apache 2.0"></a>
</p>

**geese runs schema migrations for Go services that run as many processes on one PostgreSQL database, during a rolling deploy, without downtime and without hand-written expand-and-contract migrations.**

[Documentation](https://jarensaa.github.io/geese/) · [Getting started](docs/getting-started.md) · [Go reference](https://pkg.go.dev/github.com/jarensaa/geese)

Migration tools like [goose](https://github.com/pressly/goose) apply migrations. geese decides **when**. Your processes fly as a flock: each one says, in the database, which schema versions it can serve; one leads; and the flock migrates only once every process can make the journey and has stopped what it was doing. It runs inside your application, with no Job, operator or sidecar.

## Features

- **Migrations during rolling deploys.** One version at a time, once every running process serves the next version and the rollout has settled.
- **Nothing runs old code against a new schema.** Before a migration, every process holds new requests and finishes what's in flight; held requests then run on the new code. A migration may drop what the previous release read.
- **Leader election built in**, with PostgreSQL advisory locks. If the leader dies mid-migration, the next one finishes.
- **Features switch on everywhere at once.** A capability activates when its schema is in place and every process serves it, so processes never disagree about what a client may use.
- **Stale clients reload.** The deployment says the oldest release whose clients it still serves, so a cached web app never calls an API that's gone.
- **Fails closed.** A process that loses contact with the database cancels its work before the others stop waiting for it.
- **Transport-agnostic.** `net/http` support is included; gRPC, queues and jobs use the same few calls.
- **Guard rails in CI.** A command that holds your repository to its migration window, and a test that proves rolling migrations into a baseline changes no schema.

## Install

```sh
go get github.com/jarensaa/geese
```

Requires Go 1.26 or newer and PostgreSQL. Migrations are SQL files run with [pressly/goose](https://github.com/pressly/goose).

## Quick start

A service with one migration, from [`examples/quickstart`](examples/quickstart):

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
		Weather:     geese.ClearSkies{}, // under Kubernetes, the Deployment's rollout: kube.InCluster
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

with its flight plan, `flightplan.json`, beside it:

```json
{"maxActiveVersions": 4, "minimumSupportedVersion": 1, "latestMigrationVersion": 1, "minimumServeVersion": 1, "maximumServeVersion": 1}
```

Run `go mod tidy`, then `quickstart init` once to set up an empty database, then start as many processes as you like (`ADDR` gives each its own port). From then on, deploy new releases as usual: the flock migrates when it's safe. [Getting started](docs/getting-started.md) runs it and ships a second release; [adopting geese](docs/adopting.md) covers the rest: background work, Kubernetes, capabilities and CI.

## How it works

Every two seconds, each process (a *goose*) writes a row in the database saying which schema versions it serves. One goose holds an advisory lock and *leads*, but only once the deployment has settled. When every goose it hears serves the next version, the leader sets a *heading*. Every goose *gathers*: it holds new requests and finishes those in flight. The leader migrates, and the held requests run on the new code. The whole stop takes a few seconds.

Read [how geese works](docs/concepts.md) for the details, and [the protocol](PROTOCOL.md) for exactly what the processes write and assume of each other.

## How it compares

| | Applies migrations | Knows which releases are running | Waits for in-flight work | Switches features on |
| --- | :---: | :---: | :---: | :---: |
| goose, golang-migrate, Flyway, Liquibase | ✓ | | | |
| Atlas | ✓ | | | |
| pgroll | ✓, through versioned views | | | |
| Helm hooks, Argo sync waves | as a Job | | | |
| Feature flags | | | | ✓ |
| **geese** | through goose | ✓ | ✓ | ✓ |

pgroll is the closest: it keeps old and new schemas usable side by side through views. geese keeps one schema, and coordinates the processes instead.

## Packages

| Package | |
| --- | --- |
| [`geese`](https://pkg.go.dev/github.com/jarensaa/geese) | The flock: `Join`, `Fly`, `Admit`, `Capabilities`, and the flight plan. |
| [`geesehttp`](https://pkg.go.dev/github.com/jarensaa/geese/geesehttp) | For `net/http`: admits requests, serves the capabilities, gates a route on a capability. |
| [`kube`](https://pkg.go.dev/github.com/jarensaa/geese/kube) | Waits for a Kubernetes Deployment's rollout before migrating. |
| [`goose`](https://pkg.go.dev/github.com/jarensaa/geese/goose) | pressly/goose migrations as the flock's migrator, and initializing a fresh database. |
| [`geesetest`](https://pkg.go.dev/github.com/jarensaa/geese/geesetest) | PostgreSQL in Docker for tests, one database per test. |
| [`goose/goosetest`](https://pkg.go.dev/github.com/jarensaa/geese/goose/goosetest) | Proves a rollup of migrations into a baseline changes no schema. |
| [`cmd/geese`](https://pkg.go.dev/github.com/jarensaa/geese/cmd/geese) | Checks a repository against its migration window, in CI and pre-commit. |

## Documentation

The guides are on [the documentation site](https://jarensaa.github.io/geese/), and in [`docs/`](docs):

- [Getting started](docs/getting-started.md)
- [How geese works](docs/concepts.md)
- [Adopting geese](docs/adopting.md)
- [Capabilities and the oldest page served](docs/capabilities.md)
- [Transports: HTTP, gRPC and more](docs/transports.md)
- [The protocol](PROTOCOL.md)
- Design decisions: [live schema migrations](docs/decisions/0001-live-schema-migrations.md), [capabilities](docs/decisions/0002-capabilities.md), [the oldest page served](docs/decisions/0003-oldest-page-served.md)

## Status

geese is young. It's the coordinator of a service that runs in production, extracted into a library, and its Go API may change before 1.0. The protocol the processes speak is versioned on its own, and every change to it is safe to roll out ([the protocol](PROTOCOL.md#changing-the-protocol)).

## Contributing

Issues and pull requests are welcome. The integration tests need Docker:

```sh
go test ./...                    # unit tests
go test -tags integration ./...  # against PostgreSQL in Docker
```

## License

[Apache 2.0](https://github.com/jarensaa/geese/blob/main/LICENSE)
