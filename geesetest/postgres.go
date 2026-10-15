// Package geesetest runs PostgreSQL for tests of a flock, and of the
// application that flies in it.
//
// A test package shares one PostgreSQL server in Docker (Testcontainers),
// started when a test first needs it, and each test gets a database of its
// own on it. Advisory locks and notifications are per database, so tests on
// one server can't see each other, and run side by side. A package that uses
// it runs its tests through Main, which stops the server.
package geesetest

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

// Image is the PostgreSQL the tests run on.
var Image = "postgres:17-alpine"

var server struct {
	once      sync.Once
	container *postgres.PostgresContainer
	config    *pgx.ConnConfig
	admin     *sql.DB
	err       error
}

// Main runs a package's tests, then stops its PostgreSQL.
func Main(m *testing.M) {
	code := m.Run()
	if server.admin != nil {
		server.admin.Close()
	}
	if server.container != nil {
		server.container.Terminate(context.Background())
	}
	os.Exit(code)
}

func start(ctx context.Context) error {
	server.once.Do(func() {
		container, err := postgres.Run(ctx, Image, postgres.WithDatabase("geese"), postgres.WithUsername("geese"), postgres.WithPassword("geese"), postgres.BasicWaitStrategies())
		if err != nil {
			server.err = err
			return
		}
		server.container = container
		url, err := container.ConnectionString(ctx, "sslmode=disable")
		if err != nil {
			server.err = err
			return
		}
		if server.config, server.err = pgx.ParseConfig(url); server.err != nil {
			return
		}
		server.admin = stdlib.OpenDB(*server.config)
	})
	return server.err
}

// Start makes an empty database, as production's, and connects to it. It's
// gone when the test ends.
func Start(t *testing.T, ctx context.Context) (*sql.DB, string) {
	t.Helper()
	return create(t, ctx, "")
}

// StartFrom makes a database that is a copy of a template's.
func StartFrom(t *testing.T, ctx context.Context, template *Template) (*sql.DB, string) {
	t.Helper()
	if err := template.ready(ctx); err != nil {
		t.Fatal(err)
	}
	return create(t, ctx, template.name)
}

func create(t *testing.T, ctx context.Context, template string) (*sql.DB, string) {
	t.Helper()
	if err := start(ctx); err != nil {
		t.Fatal(err)
	}
	name := "test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	statement := `CREATE DATABASE ` + name
	if template != "" {
		statement += ` TEMPLATE ` + template
	}
	if _, err := server.admin.ExecContext(ctx, statement); err != nil {
		t.Fatal(err)
	}
	conn, url := open(name)
	t.Cleanup(func() {
		conn.Close()
		// FORCE: what the test left connected goes with it.
		server.admin.ExecContext(context.Background(), `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`)
	})
	return conn, url
}

func open(name string) (*sql.DB, string) {
	config := server.config.Copy()
	config.Database = name
	url := fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable", config.User, config.Password, config.Host, config.Port, name)
	return stdlib.OpenDB(*config), url
}

// Template is a database set up once, the first time a test asks for it, for
// tests to start from a copy of.
type Template struct {
	name    string
	prepare func(ctx context.Context, conn *sql.DB, url string) error
	once    sync.Once
	err     error
}

// NewTemplate names a template and how it's set up.
func NewTemplate(name string, prepare func(ctx context.Context, conn *sql.DB, url string) error) *Template {
	return &Template{name: "template_" + name, prepare: prepare}
}

func (template *Template) ready(ctx context.Context) error {
	template.once.Do(func() {
		if template.err = start(ctx); template.err != nil {
			return
		}
		if _, template.err = server.admin.ExecContext(ctx, `CREATE DATABASE `+template.name); template.err != nil {
			return
		}
		conn, url := open(template.name)
		template.err = template.prepare(ctx, conn, url)
		conn.Close()
		if template.err != nil {
			return
		}
		// A database is copied only while nobody is connected to it.
		_, template.err = server.admin.ExecContext(ctx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname=$1 AND pid<>pg_backend_pid()`, template.name)
	})
	return template.err
}
