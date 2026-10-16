package geese

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

// Weather says whether the flock may set out: a migration, a capability's
// activation or a raised floor waits for calm. Under Kubernetes, the weather is
// calm once the Deployment has rolled out every replica, so no goose of an
// older image is still coming (package kube).
type Weather interface {
	Calm(context.Context) (bool, error)
}

// ClearSkies is the weather of a flock nothing else rolls out: always calm.
// For a single process, and for tests.
type ClearSkies struct{}

func (ClearSkies) Calm(context.Context) (bool, error) { return true, nil }

// Window is the schema versions an image serves, and the latest it can
// migrate to. It comes from the flight plan (FlightPlan.Window).
type Window struct {
	ServeFrom int64
	ServeTo   int64
	Latest    int64
}

func (w Window) valid() bool {
	return w.ServeFrom >= 1 && w.ServeTo >= w.ServeFrom && w.Latest >= w.ServeFrom && w.Latest <= w.ServeTo
}

// Capability is something the API promises once its schema is in place and
// every goose in the flock serves it: a feature switches on for the whole
// deployment at once. Ready may check a completed backfill or another
// prerequisite, in the activation's transaction.
type Capability struct {
	Name          string
	MinimumSchema int64
	Ready         func(context.Context, *sql.Tx) (bool, error)
}

// Migrator is the application's migrations: geese decides when to run them,
// and the migrator runs them. Version reads the schema version applied, in
// the transaction given; Change says what the migration to target does to
// code already serving; MigrateTo migrates to exactly the target.
//
// A MigrateTo that fails must leave the schema as it was, as a transactional
// migration does: the flock goes on at the version applied. Two calls of
// MigrateTo may overlap, as when a leader's lock went with its connection
// while it migrated and another took over: they must apply each migration
// once, as a lock held while migrating does. Package goose is a Migrator for
// pressly/goose.
type Migrator interface {
	Version(ctx context.Context, tx *sql.Tx) (int64, error)
	Change(target int64) (Change, error)
	MigrateTo(ctx context.Context, db *sql.DB, target int64) error
}

// Change is what a migration does to code already serving the schema before
// it. The flock serves throughout every migration it runs, so it runs only
// those that keep that code working.
type Change struct {
	// Disrupts says why the migration could break code already serving, or
	// hold it up: the flock won't run it. Empty, the migration keeps that code
	// working, as adding a table or a nullable column does.
	Disrupts string
	// Drops are the tables ("invites") and columns ("lists.archived") it
	// drops: it runs once every goose heard declares each unused.
	Drops []string
	// Concurrently says it builds an index without blocking writes, waiting
	// for the transactions before it to end: it isn't held to a lock timeout.
	Concurrently bool
}

// Config is what a process brings to the flock.
type Config struct {
	DatabaseURL string
	// Name says who this goose is, such as its pod's UID, for whoever reads
	// geese_flock. Geese tell each other apart by an id of their own.
	Name string
	// Release is the image's release (SemVer, as the container build sets
	// it). Its major says which pages it serves; a build that isn't a release
	// ("dev", or a pre-release) says none.
	Release  string
	Window   Window
	Weather  Weather
	Migrator Migrator
	// Capabilities are what this image's API promises once they're active.
	Capabilities []Capability
	// Retired names capabilities a baseline has made unconditional. A deployed
	// database and older geese still name them active, so this image accepts
	// and advertises them, but never reports them: nothing checks them any more.
	Retired []string
	// Unused are the tables ("invites") and columns ("lists.archived") this
	// image's code never touches, as its flight plan declares. A migration
	// that drops them runs once every goose heard declares them unused.
	Unused []string
}

var (
	// ErrGrounded is the answer to work while this goose has no place in the
	// flock: its lease lapsed, or it can't serve the schema or capabilities
	// the flock has. It serves again once a honk is heard.
	ErrGrounded = errors.New("geese: grounded, without a place in the flock")
	// ErrInactive is the answer to a feature whose capability isn't on yet
	// (Require). A client reads it as "not yet", whatever the transport.
	ErrInactive = errors.New("geese: the capability isn't on yet")
)

// How the flock keeps together. A goose honks every renewInterval, and is
// heard for leaseFor: one that isn't heard again has left the flock. Its own
// deadline is shorter than the flock's, so it stops its work before the others
// count it gone.
const (
	renewInterval = 2 * time.Second
	leaseFor      = 10 * time.Second
	ownDeadline   = 8 * time.Second
)

// How the flock migrates while it serves. A migration waits at most
// lockTimeout for a lock, so a query arriving meanwhile waits no longer behind
// it, and well within PostgreSQL's deadlock check, so it's the migration that
// gives up, never the application's transaction. It tries again after
// lockRetry or so, lockAttempts times. An attempt that fails changes nothing,
// and the next attempt at that version waits retryAfter, twice as long after
// each failure in a row, up to retryLimit.
const (
	lockTimeout  = 50 * time.Millisecond
	lockRetry    = 250 * time.Millisecond
	lockAttempts = 20
	retryAfter   = 30 * time.Second
	retryLimit   = 10 * time.Minute
)

// retryIn is how long the next attempt waits after failures in a row.
func retryIn(failures int) time.Duration {
	wait := retryAfter
	for range failures - 1 {
		if wait *= 2; wait >= retryLimit {
			return retryLimit
		}
	}
	return wait
}

// Flock is this process's place in the flock: one per process. Every unit
// of database work enters through Admit; a goose that can't keep its place
// cancels what it admitted.
type Flock struct {
	db           *sql.DB
	id           string
	name         string
	release      sql.NullInt64
	window       Window
	weather      Weather
	migrator     Migrator
	capabilities []Capability
	retired      map[string]bool
	unused       []string
	dbConfig     *pgx.ConnConfig

	ready        atomic.Bool
	deadline     atomic.Pointer[time.Time]
	version      atomic.Int64
	headingSet   atomic.Bool
	mu           sync.Mutex
	leaseTimer   *time.Timer
	leaseEpoch   uint64
	requests     map[uint64]context.CancelFunc
	nextRequest  uint64
	leaderCancel context.CancelFunc
}

// Join takes this process's place in the flock: it checks it can serve the
// schema the database has, makes the flock's tables if they're missing, and
// honks once. It doesn't fly on its own: run Fly.
func Join(ctx context.Context, config Config) (*Flock, error) {
	if config.Weather == nil || config.Migrator == nil {
		return nil, errors.New("geese: weather and a migrator are required")
	}
	if !config.Window.valid() {
		return nil, fmt.Errorf("geese: invalid window %+v: serving from 1 or more, up to and including the latest migration", config.Window)
	}
	seen := map[string]bool{}
	for _, capability := range config.Capabilities {
		if capability.Name == "" || seen[capability.Name] || capability.MinimumSchema < 1 || capability.MinimumSchema > config.Window.Latest {
			return nil, fmt.Errorf("geese: invalid or duplicate capability %q", capability.Name)
		}
		seen[capability.Name] = true
	}
	retired := map[string]bool{}
	for _, name := range config.Retired {
		if name == "" || seen[name] {
			return nil, fmt.Errorf("geese: invalid or duplicate retired capability %q", name)
		}
		seen[name] = true
		retired[name] = true
	}
	for _, name := range config.Unused {
		if !unusedName.MatchString(name) {
			return nil, fmt.Errorf("geese: invalid unused name %q: a table or table.column", name)
		}
	}
	parsed, err := pgx.ParseConfig(config.DatabaseURL)
	if err != nil {
		return nil, err
	}
	db := stdlib.OpenDB(*parsed)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}
	f := &Flock{db: db, id: uuid.NewString(), name: config.Name, release: ReleaseMajor(config.Release), window: config.Window, weather: config.Weather,
		migrator: config.Migrator, capabilities: slices.Clone(config.Capabilities), retired: retired, unused: append([]string{}, config.Unused...),
		dbConfig: parsed, requests: map[uint64]context.CancelFunc{}}
	if err := f.bootstrap(ctx); err != nil {
		db.Close()
		return nil, err
	}
	if err := f.Honk(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return f, nil
}

// Ready says whether this goose has its place: its last honk was heard, and
// its own deadline hasn't passed. A health check reports it.
func (f *Flock) Ready() bool {
	deadline := f.deadline.Load()
	return f.ready.Load() && deadline != nil && time.Now().Before(*deadline)
}

// Version is the schema version this goose serves now. It changes as the
// flock migrates; code that differs between versions branches on it.
func (f *Flock) Version() int64 { return f.version.Load() }

// ID is this goose's id in geese_flock, which lives exactly as long as its
// place in the flock. Soft state tied to it, such as who is connected to this
// process, goes when the goose does.
func (f *Flock) ID() string { return f.id }

// Land leaves the flock: work admitted is cancelled, and the others stop
// waiting for this goose at once. A process that's stopping lands after its
// work in flight is done (see Fly).
func (f *Flock) Land() {
	f.ground()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _ = f.db.ExecContext(ctx, `DELETE FROM geese_flock WHERE id=$1`, f.id)
	_ = f.db.Close()
}

// ground takes this goose out of the air: nothing is admitted, and what was
// admitted is cancelled, until a honk is heard again.
func (f *Flock) ground() {
	f.mu.Lock()
	f.groundLocked()
	f.mu.Unlock()
}

func (f *Flock) groundLocked() {
	f.ready.Store(false)
	f.leaseEpoch++
	if f.leaseTimer != nil {
		f.leaseTimer.Stop()
		f.leaseTimer = nil
	}
	for _, cancel := range f.requests {
		cancel()
	}
	if f.leaderCancel != nil {
		f.leaderCancel()
	}
}

/* ---------- Capabilities ---------- */

// supportedNames is what this image serves: its capabilities and the retired
// ones it guarantees.
func (f *Flock) supportedNames() []string {
	names := make([]string, 0, len(f.capabilities)+len(f.retired))
	for _, capability := range f.capabilities {
		names = append(names, capability.Name)
	}
	for name := range f.retired {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func (f *Flock) supportsCapabilities(active []string) bool {
	supported := map[string]bool{}
	for _, name := range f.supportedNames() {
		supported[name] = true
	}
	for _, name := range active {
		if !supported[name] {
			return false
		}
	}
	return true
}

func (f *Flock) checkActiveCapabilities(active []string, version int64) error {
	if !f.supportsCapabilities(active) {
		return errors.New("geese: an active capability is unsupported by this image")
	}
	for _, name := range active {
		for _, capability := range f.capabilities {
			if capability.Name == name && version < capability.MinimumSchema {
				return fmt.Errorf("geese: capability %s requires schema %d, current schema is %d", name, capability.MinimumSchema, version)
			}
		}
	}
	return nil
}

func decodeCapabilities(encoded string) ([]string, error) {
	var active []string
	if err := json.Unmarshal([]byte(encoded), &active); err != nil {
		return nil, err
	}
	if active == nil {
		return []string{}, nil
	}
	return active, nil
}

// Capabilities is what the flock says to clients, as an API's capabilities
// endpoint answers it: what's switched on, and the oldest release whose pages
// it serves. It encodes as the JSON pages read (docs/capabilities.md).
type Capabilities struct {
	// Active are the capabilities switched on across the deployment, by name.
	// A client treats a name it doesn't know, or one that isn't here, as off.
	Active []string `json:"capabilities"`
	// OldestPage is the oldest release whose pages the flock serves, as
	// "8.0.0", or "" until one is set. A page of an older major release loads
	// the current one.
	OldestPage string `json:"oldestPage,omitempty"`
}

// Capabilities reads what the flock says to clients, in one snapshot, rather
// than inferring it from this goose's own view of the schema. Discovery (an
// API's capabilities endpoint, package geesehttp) and enforcement (a handler
// refusing what isn't on) use the same read. A read that fails, as when the
// request asking went away, is the caller's error; only finding that this
// image can't serve what's active grounds the goose. Honking answers for the
// database's health.
func (f *Flock) Capabilities(ctx context.Context) (Capabilities, error) {
	if !f.Ready() {
		return Capabilities{}, ErrGrounded
	}
	tx, err := f.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return Capabilities{}, err
	}
	defer tx.Rollback()
	version, err := f.migrator.Version(ctx, tx)
	if err != nil {
		return Capabilities{}, err
	}
	var encoded string
	var floor sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT to_json(active_capabilities)::text,oldest_page_major FROM geese_flight WHERE id=true`).Scan(&encoded, &floor); err != nil {
		return Capabilities{}, err
	}
	active, err := decodeCapabilities(encoded)
	if err != nil {
		f.ground()
		return Capabilities{}, err
	}
	if err = f.checkServeVersion(version); err == nil {
		err = f.checkActiveCapabilities(active, version)
	}
	if err != nil {
		f.ground()
		return Capabilities{}, err
	}
	if err = tx.Commit(); err != nil {
		return Capabilities{}, err
	}
	// The version this goose serves isn't stored here: a honk stores it before
	// held work resumes, and this read's snapshot may be older than that honk.
	return Capabilities{Active: slices.DeleteFunc(active, func(name string) bool { return f.retired[name] }), OldestPage: formatFloor(floor)}, nil
}

// ActiveCapabilities is what the flock has switched on, as Capabilities reads it.
func (f *Flock) ActiveCapabilities(ctx context.Context) ([]string, error) {
	capabilities, err := f.Capabilities(ctx)
	return capabilities.Active, err
}

// Active says whether a capability is switched on across the deployment.
func (f *Flock) Active(ctx context.Context, name string) (bool, error) {
	active, err := f.ActiveCapabilities(ctx)
	return slices.Contains(active, name), err
}

// Require is what a handler of any transport asks before it serves a feature:
// nil while its capability is on, ErrInactive while it isn't, or why what's on
// couldn't be read. The handler refuses the feature on any error, as "not yet"
// (docs/transports.md says how, for HTTP and gRPC).
func (f *Flock) Require(ctx context.Context, name string) error {
	on, err := f.Active(ctx, name)
	if err == nil && !on {
		err = fmt.Errorf("%w: %s", ErrInactive, name)
	}
	return err
}

// checkUnused refuses a migration's drops, as JSON, that this image doesn't
// declare unused.
func (f *Flock) checkUnused(dropping string) error {
	var drops []string
	if err := json.Unmarshal([]byte(dropping), &drops); err != nil {
		return err
	}
	for _, name := range drops {
		if !slices.Contains(f.unused, name) {
			return fmt.Errorf("geese: the flock is dropping %s, which this image doesn't declare unused", name)
		}
	}
	return nil
}

func (f *Flock) checkServeVersion(version int64) error {
	if version < f.window.ServeFrom || version > f.window.ServeTo {
		return fmt.Errorf("geese: database schema version %d is unsupported by this image (it serves %d–%d)", version, f.window.ServeFrom, f.window.ServeTo)
	}
	return nil
}
