// Package geese lets several processes of one application share a PostgreSQL
// database through live schema migrations, as a flock.
//
// Each process joins the flock (Join) and keeps its place by honking (Fly):
// every couple of seconds it says, in the flock's own table, which schema
// versions and capabilities its image serves. One goose leads, by advisory
// lock, and only in calm weather (a Kubernetes rollout has settled). The
// leader migrates the schema one version at a time, and only when every goose
// it hears serves the next version. A migration that could break the code
// already serving, the flock gathers for first: every goose holds new work,
// interrupts its streams and lets its work in flight finish, so nothing runs
// the old schema's code against the new schema, and held work then runs on the
// new code. One that can't, such as adding a table or a nullable column, or
// dropping what every goose declares unused (Config.Unused), applies while the
// flock serves (Classifier).
//
// The same agreement switches capabilities on for the whole deployment at once,
// and says the oldest release whose pages the flock still serves, so a page
// kept by a service worker never calls an API that's gone. Clients read both
// from Capabilities; a handler asks Require before it serves a feature
// (docs/capabilities.md).
//
// Every unit of database work enters through Admit, or AdmitStream for a
// stream. A goose that loses its place cancels what it admitted before the
// flock counts it gone.
//
// geese knows no transport: Admit, Capabilities and Require, with the errors
// ErrGrounded, ErrMigrating and ErrInactive, are all a transport needs. Package
// geesehttp is an adapter for net/http; docs/transports.md shows gRPC.
//
// geese decides when; the application's migrator runs the migrations (package
// goose adapts pressly/goose). The flight plan (FlightPlan) bounds how many
// migrations are active, and the geese command checks it in CI.
package geese
