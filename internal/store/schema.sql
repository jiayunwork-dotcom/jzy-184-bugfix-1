-- AgriHeat schema, PostgreSQL 16.
-- Daily rows are DATE (no time zone): every accumulation day is a whole day.

CREATE TABLE IF NOT EXISTS stations (
    id          BIGSERIAL PRIMARY KEY,
    code        TEXT UNIQUE NOT NULL,
    name        TEXT NOT NULL DEFAULT '',
    lat         DOUBLE PRECISION NOT NULL DEFAULT 0,
    lon         DOUBLE PRECISION NOT NULL DEFAULT 0,
    elev        DOUBLE PRECISION NOT NULL DEFAULT 0,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS varieties (
    id             BIGSERIAL PRIMARY KEY,
    name           TEXT NOT NULL,
    base_temp      DOUBLE PRECISION NOT NULL,
    ceiling_temp   DOUBLE PRECISION NOT NULL,
    stage_require  JSONB NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS plots (
    id           BIGSERIAL PRIMARY KEY,
    code         TEXT UNIQUE NOT NULL,
    name         TEXT NOT NULL DEFAULT '',
    variety_id   BIGINT NOT NULL REFERENCES varieties(id),
    sowing_date  DATE NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- One contiguous station-binding interval per plot. Exactly one open interval
-- (effective_to IS NULL) per plot; re-bind closes the old one and opens a new
-- one inside one transaction.
CREATE TABLE IF NOT EXISTS plot_bindings (
    id              BIGSERIAL PRIMARY KEY,
    plot_id         BIGINT NOT NULL REFERENCES plots(id) ON DELETE CASCADE,
    station_id      BIGINT NOT NULL REFERENCES stations(id),
    effective_from  DATE NOT NULL,
    effective_to    DATE
);
CREATE UNIQUE INDEX IF NOT EXISTS ux_binding_current
    ON plot_bindings(plot_id) WHERE effective_to IS NULL;
CREATE INDEX IF NOT EXISTS ix_binding_station
    ON plot_bindings(station_id, effective_from);

-- Authoritative daily station temperature. source='obs' for the winning
-- report (largest seq), fill_* for imputed rows.
CREATE TABLE IF NOT EXISTS weather (
    station_id  BIGINT NOT NULL REFERENCES stations(id),
    day         DATE NOT NULL,
    tmax        DOUBLE PRECISION NOT NULL,
    tmin        DOUBLE PRECISION NOT NULL,
    source      TEXT NOT NULL,
    seq         BIGINT NOT NULL DEFAULT 0,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (station_id, day)
);

-- Traceability: which observed rows a fill was blended from.
CREATE TABLE IF NOT EXISTS weather_basis (
    station_id        BIGINT NOT NULL,
    day               DATE NOT NULL,
    basis_station_id  BIGINT NOT NULL,
    basis_day         DATE NOT NULL,
    PRIMARY KEY (station_id, day, basis_station_id, basis_day)
);
CREATE INDEX IF NOT EXISTS ix_basis_lookup
    ON weather_basis(basis_station_id, basis_day);

-- Climatology table: one row per day-of-year (1..366), rebuilt from observed
-- weather after every accepted report.
CREATE TABLE IF NOT EXISTS normals (
    station_id  BIGINT NOT NULL REFERENCES stations(id),
    doy         INT NOT NULL,
    tmax        DOUBLE PRECISION NOT NULL,
    tmin        DOUBLE PRECISION NOT NULL,
    samples     INT NOT NULL,
    PRIMARY KEY (station_id, doy)
);

-- Monthly cumulative checkpoints for incremental recomputation.
CREATE TABLE IF NOT EXISTS plot_snapshots (
    plot_id   BIGINT NOT NULL REFERENCES plots(id) ON DELETE CASCADE,
    day       DATE NOT NULL,
    cum_gdd   DOUBLE PRECISION NOT NULL,
    PRIMARY KEY (plot_id, day)
);

-- Persisted daily curve for fast plot queries.
CREATE TABLE IF NOT EXISTS plot_days (
    plot_id     BIGINT NOT NULL REFERENCES plots(id) ON DELETE CASCADE,
    day         DATE NOT NULL,
    station_id  BIGINT,
    tmax        DOUBLE PRECISION,
    tmin        DOUBLE PRECISION,
    daily_gdd   DOUBLE PRECISION NOT NULL,
    cum_gdd     DOUBLE PRECISION NOT NULL,
    source      TEXT NOT NULL,
    PRIMARY KEY (plot_id, day)
);

CREATE TABLE IF NOT EXISTS plot_stages (
    plot_id  BIGINT NOT NULL REFERENCES plots(id) ON DELETE CASCADE,
    stage    TEXT NOT NULL,
    status   TEXT NOT NULL,
    day      DATE,
    cum_at   DOUBLE PRECISION NOT NULL DEFAULT 0,
    PRIMARY KEY (plot_id, stage)
);

-- (plot, stage, change_id) uniqueness makes event emission idempotent: the
-- same data change can never produce a duplicate event for a stage.
CREATE TABLE IF NOT EXISTS stage_events (
    id          BIGSERIAL PRIMARY KEY,
    plot_id     BIGINT NOT NULL REFERENCES plots(id),
    stage       TEXT NOT NULL,
    old_day     DATE,
    new_day     DATE,
    reason      TEXT NOT NULL,
    change_id   TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (plot_id, stage, change_id)
);
CREATE INDEX IF NOT EXISTS ix_events_pull ON stage_events(id);

-- Durable work queue. pending tasks survive crashes; processing tasks are
-- reset to pending on startup (restart resume). run_after defers tasks whose
-- effective date (e.g. future rebinding) has not arrived.
CREATE TABLE IF NOT EXISTS recompute_tasks (
    id           BIGSERIAL PRIMARY KEY,
    plot_id      BIGINT NOT NULL REFERENCES plots(id),
    as_of        DATE NOT NULL,
    start_day    DATE,
    change_id    TEXT NOT NULL,
    reason       TEXT NOT NULL,
    status       TEXT NOT NULL DEFAULT 'pending',
    attempts     INT NOT NULL DEFAULT 0,
    is_initial   BOOLEAN NOT NULL DEFAULT false,
    run_after    DATE,
    err          TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at   TIMESTAMPTZ,
    finished_at  TIMESTAMPTZ,
    UNIQUE (plot_id, change_id)
);
CREATE INDEX IF NOT EXISTS ix_tasks_claim
    ON recompute_tasks(status, run_after, id);
-- Supports the per-plot ordering gate in Claim so distinct queued changes for
-- one plot cannot be processed concurrently or out of id order.
CREATE INDEX IF NOT EXISTS ix_tasks_plot_active
    ON recompute_tasks(plot_id, id)
    WHERE status IN ('pending', 'processing');
