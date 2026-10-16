// Package geese lets several processes of one application share a PostgreSQL
// database through live schema migrations, as a flock.
//
// Each process joins the flock (Join) and keeps its place by honking (Fly):
// every couple of seconds it says, in the flock's own table, which schema
// versions and capabilities its image serves. One goose leads, by advisory
// lock, and only in calm weather (a Kubernetes rollout has settled). The
// leader migrates the schema one version at a time, and only when every goose
// it hears serves the next version, and declares unused what the migration
// drops (Config.Unused). The flock serves throughout: the migrator says what
// each migration changes (Change), and the leader runs only those that keep
// the code already serving working, such as adding a table or a nullable
// column. A migration that asks for downtime runs instead with the flock down:
// every goose refuses new work (ErrRoosting) and finishes what it has, first.
//
// The same agreement switches capabilities on for the whole deployment at once,
// and says the oldest release whose pages the flock still serves, so a page
// kept by a service worker never calls an API that's gone. Clients read both
// from Capabilities; a handler asks Require before it serves a feature
// (docs/capabilities.md).
//
// Every unit of database work enters through Admit. A goose that loses its
// place cancels what it admitted before the flock counts it gone.
//
// geese knows no transport: Admit, Capabilities and Require, with the errors
// ErrGrounded, ErrRoosting and ErrInactive, are all a transport needs. Package geesehttp
// is an adapter for net/http; docs/transports.md shows gRPC.
//
// geese decides when; the application's migrator runs the migrations (package
// goose adapts pressly/goose). The flight plan (FlightPlan) bounds how many
// migrations are active, and the geese command checks it in CI.
package geese
