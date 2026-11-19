-- What the env canvas's "Internet outbound" box remembers per tile: when the
-- tile last reached the internet and how many bytes that added up to since
-- this was first recorded. Last seen and totals only; no per-tick history.
CREATE TABLE tile_outbound (
    tile_id   TEXT     PRIMARY KEY REFERENCES tiles (id) ON DELETE CASCADE,
    last_seen DATETIME NOT NULL,
    bytes     INTEGER  NOT NULL DEFAULT 0
);
