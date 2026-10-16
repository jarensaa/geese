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
		pauses    string // part of why it pauses; empty when it doesn't
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
ALTER TABLE items ADD CONSTRAINT items_list FOREIGN KEY (list_id) REFERENCES lists (id) NOT VALID;
ALTER TABLE items VALIDATE CONSTRAINT items_list;`, "", nil},
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
		{"no Up", "CREATE TABLE notes (id bigint);", "no -- +goose Up", nil},
	} {
		change := Classify([]byte(c.migration))
		if c.pauses == "" && change.Pause != "" || !strings.Contains(change.Pause, c.pauses) {
			t.Errorf("%s: pauses %q, expected %q", c.name, change.Pause, c.pauses)
		}
		if c.pauses == "" && !slices.Equal(change.Drops, c.drops) {
			t.Errorf("%s: drops %q, expected %q", c.name, change.Drops, c.drops)
		}
	}
}
