-- +goose Up
CREATE TABLE items (id bigint PRIMARY KEY, list_id bigint NOT NULL REFERENCES lists(id), text text NOT NULL);

-- +goose Down
DROP TABLE items;
