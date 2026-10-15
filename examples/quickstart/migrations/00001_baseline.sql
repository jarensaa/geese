-- +goose Up
CREATE TABLE greetings (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    said text NOT NULL
);

-- +goose Down
DROP TABLE greetings;
