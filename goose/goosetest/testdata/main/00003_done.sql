-- +goose Up
ALTER TABLE items ADD COLUMN done boolean NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE items DROP COLUMN done;
