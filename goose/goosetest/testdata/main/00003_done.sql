-- +goose Up
ALTER TABLE items ADD COLUMN done boolean NOT NULL DEFAULT false;
CREATE INDEX items_list_idx ON items (list_id);

-- +goose Down
DROP INDEX items_list_idx;
ALTER TABLE items DROP COLUMN done;
