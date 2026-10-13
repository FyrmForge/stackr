-- tile_vips: the VIP a tile's pause container holds on the node it runs on.
-- The deploy pins it in the env network's lower half; the minute VIP rebuild
-- reads it from here instead of asking Docker per tile, and fixes a row that
-- drifted from what the node's one container list shows.
CREATE TABLE tile_vips (
    tile_id  TEXT  PRIMARY KEY REFERENCES tiles (id) ON DELETE CASCADE,
    node_id  TEXT  NOT NULL,
    vip      TEXT  NOT NULL
);
