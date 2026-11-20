package db

import (
	"database/sql"
	_ "embed"
	"fmt"
	"log"
	"strings"

	_ "github.com/mattn/go-sqlite3"
)

//go:embed schema.sql
var schemaSQL string

// schemaVersion is the target PRAGMA user_version. Bump it whenever a new
// migration step is appended to the migrations map below.
const schemaVersion = 1

// InitDB opens the database with WAL enabled and applies the schema. A single
// connection prevents "database is locked" while the tailer writes and the
// dashboard reads concurrently.
func InitDB(dbPath string) (*sql.DB, error) {
	dsn := fmt.Sprintf("file:%s?_journal_mode=WAL&_busy_timeout=5000&_synchronous=NORMAL", dbPath)
	database, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}
	database.SetMaxOpenConns(1)
	database.SetConnMaxLifetime(0)

	if err := database.Ping(); err != nil {
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}
	// Fresh installs get the full current schema; CREATE TABLE IF NOT EXISTS is
	// a no-op on existing databases.
	if err := applySchema(database); err != nil {
		return nil, err
	}
	// Existing databases are brought up to date additively. History older than
	// the retained logs is never dropped.
	if err := migrate(database); err != nil {
		return nil, err
	}
	return database, nil
}

// migrate applies ordered, idempotent migration steps until the stored
// user_version reaches schemaVersion. Unlike the previous drop-and-rebuild
// approach, existing rows (match history older than the retained logs) are
// preserved: every step is additive (ADD COLUMN / data-preserving rebuild).
func migrate(database *sql.DB) error {
	var version int
	if err := database.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("reading user_version: %w", err)
	}
	if version >= schemaVersion {
		return nil
	}

	for v := version; v < schemaVersion; v++ {
		step, ok := migrations[v]
		if !ok {
			continue
		}
		if err := step(database); err != nil {
			return fmt.Errorf("migration v%d failed: %w", v, err)
		}
		if _, err := database.Exec(fmt.Sprintf("PRAGMA user_version = %d", v+1)); err != nil {
			return fmt.Errorf("setting user_version to %d: %w", v+1, err)
		}
	}
	return nil
}

// migrations is keyed by the version it upgrades FROM. Key 0 moves a database
// from user_version 0 to 1.
var migrations = map[int]func(*sql.DB) error{
	0: migrateV0toV1,
}

// migrateV0toV1 ensures the columns added after the original schema exist and
// makes matches.won nullable so a match with an unknown result can be stored.
// Every step preserves existing rows.
func migrateV0toV1(database *sql.DB) error {
	steps := []struct {
		table  string
		column string
		ddl    string
	}{
		{"matches", "player_name", "ALTER TABLE matches ADD COLUMN player_name TEXT"},
		{"player_stats", "lifetime_wins", "ALTER TABLE player_stats ADD COLUMN lifetime_wins INTEGER"},
		{"player_stats", "lifetime_losses", "ALTER TABLE player_stats ADD COLUMN lifetime_losses INTEGER"},
		{"decks", "deck_id", "ALTER TABLE decks ADD COLUMN deck_id TEXT"},
	}
	for _, s := range steps {
		missing, err := tableMissingColumn(database, s.table, s.column)
		if err != nil {
			return err
		}
		if !missing {
			continue
		}
		log.Printf("[DB] migration: adding %s.%s", s.table, s.column)
		if _, err := database.Exec(s.ddl); err != nil {
			return fmt.Errorf("adding %s.%s: %w", s.table, s.column, err)
		}
	}

	// matches.won was NOT NULL in the original schema; relax it so a match whose
	// result is unknown can still be recorded. Rebuild the table and copy every
	// row so no history is lost.
	notNull, err := columnNotNull(database, "matches", "won")
	if err != nil {
		return err
	}
	if notNull {
		log.Printf("[DB] migration: making matches.won nullable (preserving rows)")
		return rebuildMatchesNullableWon(database)
	}
	return nil
}

// rebuildMatchesNullableWon recreates the matches table with a nullable won
// column, copying all existing rows across inside one transaction.
func rebuildMatchesNullableWon(database *sql.DB) error {
	tx, err := database.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmts := []string{
		`CREATE TABLE matches_new (
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
		)`,
		`INSERT INTO matches_new (
			match_id, player_name, deck_id, deck_name, on_play, won,
			my_team_id, winning_team_id, rank_class, rank_level,
			rank_percentile, ended_at, source_log, created_at
		)
		SELECT
			match_id, player_name, deck_id, deck_name, on_play, won,
			my_team_id, winning_team_id, rank_class, rank_level,
			rank_percentile, ended_at, source_log, created_at
		FROM matches`,
		`DROP TABLE matches`,
		`ALTER TABLE matches_new RENAME TO matches`,
		`CREATE INDEX IF NOT EXISTS idx_matches_deck   ON matches(deck_id)`,
		`CREATE INDEX IF NOT EXISTS idx_matches_ended  ON matches(ended_at)`,
		`CREATE INDEX IF NOT EXISTS idx_matches_player ON matches(player_name)`,
	}
	for _, s := range stmts {
		if _, err := tx.Exec(s); err != nil {
			return fmt.Errorf("rebuilding matches: %w", err)
		}
	}
	return tx.Commit()
}

// columnNotNull reports whether the named column exists and is declared
// NOT NULL.
func columnNotNull(database *sql.DB, table, column string) (bool, error) {
	rows, err := database.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			cid        int
			name, typ  string
			notNull    int
			dflt       sql.NullString
			primaryKey int
		)
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &primaryKey); err != nil {
			return false, err
		}
		if name == column {
			return notNull == 1, nil
		}
	}
	return false, rows.Err()
}

// tableMissingColumn reports whether the table exists and lacks the column.
// A table that does not exist yet is not stale.
func tableMissingColumn(database *sql.DB, table, column string) (bool, error) {
	cols, err := tableColumns(database, table)
	if err != nil {
		return false, err
	}
	if len(cols) == 0 {
		return false, nil
	}
	_, ok := cols[column]
	return !ok, nil
}

func applySchema(database *sql.DB) error {
	for _, stmt := range strings.Split(schemaSQL, ";") {
		stmt = strings.TrimSpace(stmt)
		for strings.HasPrefix(stmt, "--") {
			if i := strings.IndexByte(stmt, '\n'); i >= 0 {
				stmt = strings.TrimSpace(stmt[i+1:])
			} else {
				stmt = ""
			}
		}
		if stmt == "" {
			continue
		}
		if _, err := database.Exec(stmt); err != nil {
			return fmt.Errorf("failed to apply schema near %q: %w", firstLine(stmt), err)
		}
	}
	return nil
}

func tableColumns(database *sql.DB, table string) (map[string]struct{}, error) {
	rows, err := database.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	cols := make(map[string]struct{})
	for rows.Next() {
		var (
			cid        int
			name, typ  string
			notNull    int
			dflt       sql.NullString
			primaryKey int
		)
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &primaryKey); err != nil {
			return nil, err
		}
		cols[name] = struct{}{}
	}
	return cols, rows.Err()
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
