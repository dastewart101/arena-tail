-- Single source of truth for the schema. db.go embeds this file.

-- Decks are recorded only when the player actually selects one for an event.
CREATE TABLE IF NOT EXISTS decks (
    deck_id       TEXT PRIMARY KEY,
    name          TEXT NOT NULL,
    format        TEXT,
    first_seen_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    last_seen_at  DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- One row per completed match. match_id is the dedupe key across log files.
-- won is nullable: a match observed without a result (e.g. mid-match start) is
-- stored with an unknown result rather than discarded.
CREATE TABLE IF NOT EXISTS matches (
    match_id        TEXT PRIMARY KEY,
    player_name     TEXT NOT NULL,
    deck_id         TEXT,
    deck_name       TEXT,
    on_play         INTEGER,
    won             INTEGER,
    my_team_id      INTEGER,
    winning_team_id INTEGER,
    rank_class      TEXT,
    rank_level      INTEGER,
    rank_percentile REAL,
    ended_at        DATETIME,
    source_log      TEXT,
    created_at      DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- Identity plus rank and MTGA's own lifetime counters. The counters come from
-- the RankGetCombinedRankInfo payload, so they match what the game displays.
CREATE TABLE IF NOT EXISTS player_stats (
    screen_name            TEXT PRIMARY KEY,
    best_mythic_percentile REAL,
    best_mythic_rank       INTEGER,
    last_class             TEXT,
    last_level             INTEGER,
    last_percentile        REAL,
    lifetime_wins          INTEGER,
    lifetime_losses        INTEGER,
    season_ordinal         INTEGER,
    updated_at             DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_matches_deck   ON matches(deck_id);
CREATE INDEX IF NOT EXISTS idx_matches_ended  ON matches(ended_at);
CREATE INDEX IF NOT EXISTS idx_matches_player ON matches(player_name);

