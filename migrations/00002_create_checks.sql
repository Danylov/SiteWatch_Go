-- +goose Up
CREATE TABLE checks (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    site_id     BIGINT      NOT NULL REFERENCES sites (id) ON DELETE CASCADE,
    status_code INT         NOT NULL,
    latency_ms  BIGINT      NOT NULL,
    error       TEXT        NOT NULL DEFAULT '',
    checked_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX checks_site_id_checked_at_idx ON checks (site_id, checked_at DESC);

-- +goose Down
DROP TABLE checks;
