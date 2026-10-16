-- +goose Up
CREATE TABLE items (id bigint PRIMARY KEY, list_id bigint NOT NULL REFERENCES lists(id), text text NOT NULL);
CREATE INDEX items_list_idx ON items (list_id);

-- +goose Down
DROP TABLE items;
