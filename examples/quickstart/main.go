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
