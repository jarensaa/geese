-- +goose Up
CREATE TABLE lists (id bigint PRIMARY KEY, name text NOT NULL);

-- +goose Down
DROP TABLE lists;
