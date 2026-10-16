package goose

import (
	"slices"
	"strings"
	"testing"
)

func TestClassify(t *testing.T) {
	for _, c := range []struct {
		name      string
		migration string
		disrupts  string // part of why it disrupts the flock; empty when it doesn't
		drops     []string
	}{
		{"a new table, indexed", `-- +goose Up
CREATE TABLE notes (id bigint PRIMARY KEY, list_id bigint NOT NULL REFERENCES lists(id), body text NOT NULL);
CREATE INDEX notes_list ON notes (list_id);
-- +goose Down
DROP TABLE notes;`, "", nil},
		{"a nullable column, and one with a constant default", `-- +goose Up
ALTER TABLE lists ADD COLUMN colour text;
ALTER TABLE lists ADD COLUMN IF NOT EXISTS language text NOT NULL DEFAULT 'en';
ALTER TABLE lists ADD archived boolean NOT NULL DEFAULT false;
ALTER TABLE lists ADD COLUMN price numeric(10,2) DEFAULT 0;`, "", nil},
		{"types, sequences, comments and constraints not yet valid", `-- +goose Up
CREATE TYPE mood AS ENUM ('sad', 'ok');
ALTER TYPE mood ADD VALUE 'happy';
CREATE SEQUENCE tickets;
COMMENT ON TABLE lists IS 'Lists; of things';
ALTER TABLE items ADD CONSTRAINT items_list FOREIGN KEY (list_id) REFERENCES lists (id) NOT VALID;`, "", nil},
		{"validating a constraint, on its own", "-- +goose Up\nALTER TABLE items VALIDATE CONSTRAINT items_list;", "", nil},
		{"drops", `-- +goose Up
-- The archive moved to its own table.
ALTER TABLE lists DROP COLUMN archived;
ALTER TABLE "Lists" DROP "Colour";
DROP TABLE IF EXISTS invites, app.tokens;`, "", []string{"lists.archived", "Lists.Colour", "invites", "app.tokens"}},
		{"a table made and dropped in one migration drops nothing old code reads", `-- +goose Up
CREATE TABLE scratch (id bigint);
DROP TABLE scratch;`, "", nil},

		{"what's in a literal or a quoted name is only that", `-- +goose Up
ALTER TABLE lists ADD COLUMN note text DEFAULT 'not null; DROP TABLE lists';
CREATE TABLE "odd;name" (id bigint);`, "", nil},

		{"a cascade on a table", "-- +goose Up\nDROP TABLE invites CASCADE;", "can't tell", nil},
		{"a constraint checked as it's added", "-- +goose Up\nALTER TABLE lists ADD CONSTRAINT named UNIQUE (name);", "can't tell", nil},
		{"validating a constraint beside another change", `-- +goose Up
ALTER TABLE lists ADD COLUMN colour text;
ALTER TABLE items VALIDATE CONSTRAINT items_list;`, "a migration of its own", nil},
		{"an index on a table old code writes", "-- +goose Up\nCREATE INDEX lists_name ON lists (name);", "blocks writes", nil},
		{"a default that isn't a constant", "-- +goose Up\nALTER TABLE lists ADD COLUMN token uuid DEFAULT gen_random_uuid();", "rewrites the table", nil},
		{"a NOT NULL column without a default", "-- +goose Up\nALTER TABLE lists ADD COLUMN owner bigint NOT NULL;", "NOT NULL", nil},
		{"a column with a constraint that checks every row", "-- +goose Up\nALTER TABLE lists ADD COLUMN owner bigint REFERENCES people (id);", "checks every row", nil},
		{"two changes in one statement", "-- +goose Up\nALTER TABLE lists ADD COLUMN a text, ADD COLUMN b text;", "more than one change", nil},
		{"a rename", "-- +goose Up\nALTER TABLE lists RENAME COLUMN name TO title;", "can't tell", nil},
		{"a cascade", "-- +goose Up\nALTER TABLE lists DROP COLUMN archived CASCADE;", "can't tell", nil},
		{"data", "-- +goose Up\nUPDATE lists SET name = 'x';", "can't tell", nil},
		{"a function", "-- +goose Up\n-- +goose StatementBegin\nCREATE FUNCTION f() RETURNS int AS $$ SELECT 1; $$ LANGUAGE sql;\n-- +goose StatementEnd", "StatementBegin", nil},
		{"dollar quotes", "-- +goose Up\nCOMMENT ON TABLE lists IS $$x$$;", "dollar", nil},
		{"outside a transaction", "-- +goose NO TRANSACTION\n-- +goose Up\nCREATE TABLE notes (id bigint);", "outside a transaction", nil},
		{"an index built concurrently, without its leftovers dropped first", "-- +goose NO TRANSACTION\n-- +goose Up\nCREATE INDEX CONCURRENTLY lists_name ON lists (name);", "outside a transaction", nil},
		{"an index built concurrently, if it isn't there", "-- +goose NO TRANSACTION\n-- +goose Up\nDROP INDEX CONCURRENTLY IF EXISTS lists_name;\nCREATE INDEX CONCURRENTLY IF NOT EXISTS lists_name ON lists (name);", "outside a transaction", nil},
		{"another index's leftovers dropped", "-- +goose NO TRANSACTION\n-- +goose Up\nDROP INDEX CONCURRENTLY IF EXISTS other;\nCREATE INDEX CONCURRENTLY lists_name ON lists (name);", "outside a transaction", nil},
		{"no Up", "CREATE TABLE notes (id bigint);", "which goose needs to run it", nil},
	} {
		change := Classify([]byte(c.migration))
		if c.disrupts == "" && change.Disrupts != "" || !strings.Contains(change.Disrupts, c.disrupts) {
			t.Errorf("%s: disrupts %q, expected %q", c.name, change.Disrupts, c.disrupts)
		}
		if change.Concurrently {
			t.Errorf("%s: concurrently", c.name)
		}
		if c.disrupts == "" && !slices.Equal(change.Drops, c.drops) {
			t.Errorf("%s: drops %q, expected %q", c.name, change.Drops, c.drops)
		}
	}
}

func TestClassifyAnIndexBuiltConcurrently(t *testing.T) {
	change := Classify([]byte(`-- +goose NO TRANSACTION
-- +goose Up
DROP INDEX CONCURRENTLY IF EXISTS app.lists_name;
CREATE UNIQUE INDEX CONCURRENTLY lists_name ON app.lists (name);
-- +goose Down
DROP INDEX CONCURRENTLY lists_name;`))
	if change.Disrupts != "" || !change.Concurrently || len(change.Drops) != 0 {
		t.Fatalf("%+v", change)
	}
}

func TestClassifyAMigrationWithDowntime(t *testing.T) {
	for _, c := range []struct {
		name, migration, disrupts string
	}{
		{"a rename", "-- +geese downtime\n-- +goose Up\nALTER TABLE lists RENAME name TO title;", ""},
		{"data, and a function", "-- +goose Up\n-- +geese downtime\nUPDATE lists SET name = lower(name);\n-- +goose StatementBegin\nCREATE FUNCTION f() RETURNS int AS $$ SELECT 1; $$ LANGUAGE sql;\n-- +goose StatementEnd", ""},
		{"outside a transaction", "-- +geese downtime\n-- +goose NO TRANSACTION\n-- +goose Up\nALTER TABLE lists RENAME name TO title;", "in a transaction"},
		{"no Up", "-- +geese downtime\nALTER TABLE lists RENAME name TO title;", "which goose needs to run it"},
		{"an annotation geese doesn't know", "-- +geese sometimes\n-- +goose Up\nCREATE TABLE notes (id bigint);", "doesn't know"},
	} {
		change := Classify([]byte(c.migration))
		if c.disrupts == "" && (change.Disrupts != "" || !change.Downtime) || c.disrupts != "" && (!strings.Contains(change.Disrupts, c.disrupts) || change.Downtime) {
			t.Errorf("%s: %+v, expected disrupts %q", c.name, change, c.disrupts)
		}
	}
}
