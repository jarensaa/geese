-- +goose Up
-- Migrations 1 to 3, rolled into one.
CREATE TABLE lists (id bigint PRIMARY KEY, name text NOT NULL);
CREATE TABLE items (id bigint PRIMARY KEY, list_id bigint NOT NULL REFERENCES lists(id), text text NOT NULL, done boolean NOT NULL DEFAULT false);
CREATE INDEX items_list_idx ON items (list_id);

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'the baseline cannot be rolled back'; END $$;
-- +goose StatementEnd
