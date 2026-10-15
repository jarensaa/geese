// Package geese lets several processes of one application share a PostgreSQL
// database through live schema migrations, as a flock.
//
// Each process joins the flock (Join) and keeps its place by honking (Fly):
// every couple of seconds it says, in the flock's own table, which schema
// versions and capabilities its image serves. One goose leads, by advisory
// lock, and only in calm weather (a Kubernetes rollout has settled). The
// leader migrates the schema one version at a time, and only when every goose
// it hears serves the next version. Before it does, the flock gathers: every
// goose holds new work, interrupts its streams and lets its work in flight
// finish, so nothing runs the old schema's code against the new schema. Held
// work then runs on the new code. A migration may therefore drop what the
// previous schema's code read, as long as no image that needs it is still
// flying.
//
// The same agreement switches capabilities on for the whole deployment at once
// (ActiveCapabilities), and says the oldest release whose pages the flock still
// serves (OldestPage), so a page kept by a service worker never calls an API
// that's gone.
//
// Every unit of database work enters through Admit, or AdmitStream for a
// socket; package geesehttp does it for HTTP. A goose that loses its place
// cancels what it admitted before the flock counts it gone.
//
// geese decides when; the application's migrator runs the migrations (package
// goose adapts pressly/goose). The flight plan (FlightPlan) bounds how many
// migrations are active, and the geese command checks it in CI.
package geese
