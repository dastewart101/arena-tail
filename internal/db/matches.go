package db

import (
	"database/sql"
	"fmt"
	"time"
)

// Deck is a deck the player has actually selected for an event. The catalogue
// of unused decks in the logs is intentionally never imported.
type Deck struct {
	DeckID string
	Name   string
	Format string
}

// UpsertDeck records a deck selection, creating the row on first sight and
// refreshing the name if the player renamed it in Arena.
func UpsertDeck(database *sql.DB, d Deck) error {
	_, err := database.Exec(`
		INSERT INTO decks (deck_id, name, format)
		VALUES (?, ?, ?)
		ON CONFLICT(deck_id) DO UPDATE SET
			name = excluded.name,
			format = COALESCE(excluded.format, decks.format),
			last_seen_at = CURRENT_TIMESTAMP`,
		d.DeckID, d.Name, d.Format)
	if err != nil {
		return fmt.Errorf("failed to upsert deck %s: %w", d.DeckID, err)
	}
	return nil
}

// MatchRecord is a completed match ready to persist. Won and MyTeamID are
// pointers so a match observed without a result (or without seat information)
// can be stored with unknown values instead of being discarded.
type MatchRecord struct {
	MatchID        string
	PlayerName     string
	DeckID         string
	DeckName       string
	OnPlay         *bool
	Won            *bool
	MyTeamID       *int
	WinningTeamID  *int
	RankClass      string
	RankLevel      int
	RankPercentile float64
	EndedAt        time.Time
	SourceLog      string
}

// InsertMatch stores one match. match_id is the primary key, so replaying a log
// or scanning both logs cannot create duplicates. Reports whether the row was
// newly inserted.
func InsertMatch(database *sql.DB, m MatchRecord) (bool, error) {
	var onPlay any
	if m.OnPlay != nil {
		onPlay = boolToInt(*m.OnPlay)
	}
	var won any
	if m.Won != nil {
		won = boolToInt(*m.Won)
	}
	var myTeam, winTeam any
	if m.MyTeamID != nil {
		myTeam = *m.MyTeamID
	}
	if m.WinningTeamID != nil {
		winTeam = *m.WinningTeamID
	}

	ended := any(nil)
	if !m.EndedAt.IsZero() {
		ended = m.EndedAt.UTC()
	}

	res, err := database.Exec(`
		INSERT INTO matches (
			match_id, player_name, deck_id, deck_name, on_play, won,
			my_team_id, winning_team_id, rank_class, rank_level,
			rank_percentile, ended_at, source_log
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(match_id) DO NOTHING`,
		m.MatchID, m.PlayerName, nullable(m.DeckID), nullable(m.DeckName),
		onPlay, won, myTeam, winTeam,
		nullable(m.RankClass), m.RankLevel, m.RankPercentile,
		ended, m.SourceLog)
	if err != nil {
		return false, fmt.Errorf("failed to insert match %s: %w", m.MatchID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// RecordRank stores the latest rank plus MTGA's own lifetime win/loss counters.
// The counters are absolute values from the game, so they overwrite rather than
// accumulate; percentile is the exception, where higher is better.
func RecordRank(database *sql.DB, screenName, class string, level int, percentile float64,
	wins, losses, seasonOrdinal int) error {
	_, err := database.Exec(`
		INSERT INTO player_stats (
			screen_name, last_class, last_level, last_percentile,
			best_mythic_percentile, lifetime_wins, lifetime_losses, season_ordinal)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(screen_name) DO UPDATE SET
			last_class = excluded.last_class,
			last_level = excluded.last_level,
			last_percentile = excluded.last_percentile,
			lifetime_wins = excluded.lifetime_wins,
			lifetime_losses = excluded.lifetime_losses,
			season_ordinal = excluded.season_ordinal,
			best_mythic_percentile = MAX(
				COALESCE(player_stats.best_mythic_percentile, 0),
				COALESCE(excluded.best_mythic_percentile, 0)
			),
			updated_at = CURRENT_TIMESTAMP`,
		screenName, class, level, percentile, percentile, wins, losses, seasonOrdinal)
	if err != nil {
		return fmt.Errorf("failed to record rank: %w", err)
	}
	return nil
}

// Totals is lifetime win/loss.
type Totals struct {
	Wins   int
	Losses int
}

// PostureTotals is the win/loss split for being on the play versus the draw.
type PostureTotals struct {
	PlayWins   int
	PlayLosses int
	DrawWins   int
	DrawLosses int
}

// DeckTotals is per-deck performance, including its own play/draw split.
type DeckTotals struct {
	DeckID     string
	DeckName   string
	Wins       int
	Losses     int
	PlayWins   int
	PlayLosses int
	DrawWins   int
	DrawLosses int
}

// RankSnapshot is the current rank, MTGA's lifetime counters and the best
// Mythic percentile reached.
type RankSnapshot struct {
	Class          string
	Level          int
	Percentile     float64
	LifetimeWins   int
	LifetimeLosses int
	SeasonOrdinal  int
	BestMythicPct  float64
	HasBestMythic  bool
}

// LifetimeTotals is derived from matches, so re-scanning a log cannot inflate
// it the way the previous running counters did.
func LifetimeTotals(database *sql.DB, screenName string) (Totals, error) {
	var t Totals
	err := database.QueryRow(`
		SELECT COALESCE(SUM(won), 0), COALESCE(SUM(1 - won), 0)
		FROM matches WHERE player_name = ?`, screenName).
		Scan(&t.Wins, &t.Losses)
	return t, err
}

func Posture(database *sql.DB, screenName string) (PostureTotals, error) {
	var p PostureTotals
	err := database.QueryRow(`
		SELECT
			COALESCE(SUM(CASE WHEN on_play = 1 AND won = 1 THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN on_play = 1 AND won = 0 THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN on_play = 0 AND won = 1 THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN on_play = 0 AND won = 0 THEN 1 ELSE 0 END), 0)
		FROM matches WHERE player_name = ?`, screenName).
		Scan(&p.PlayWins, &p.PlayLosses, &p.DrawWins, &p.DrawLosses)
	return p, err
}

// DecksPlayed returns per-deck results, most-played first. A limit <= 0 returns
// every deck. Matches with no identified deck are grouped under a single
// placeholder.
func DecksPlayed(database *sql.DB, screenName string, limit int) ([]DeckTotals, error) {
	query := `
		SELECT
			COALESCE(m.deck_id, ''),
			COALESCE(d.name, m.deck_name, 'Unknown Deck'),
			COALESCE(SUM(won), 0),
			COALESCE(SUM(1 - won), 0),
			COALESCE(SUM(CASE WHEN on_play = 1 AND won = 1 THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN on_play = 1 AND won = 0 THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN on_play = 0 AND won = 1 THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN on_play = 0 AND won = 0 THEN 1 ELSE 0 END), 0)
		FROM matches m
		LEFT JOIN decks d ON d.deck_id = m.deck_id
		WHERE m.player_name = ?
		GROUP BY COALESCE(m.deck_id, '')
		ORDER BY COUNT(*) DESC`

	args := []any{screenName}
	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	}

	rows, err := database.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []DeckTotals
	for rows.Next() {
		var d DeckTotals
		if err := rows.Scan(&d.DeckID, &d.DeckName, &d.Wins, &d.Losses,
			&d.PlayWins, &d.PlayLosses, &d.DrawWins, &d.DrawLosses); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func Rank(database *sql.DB, screenName string) (RankSnapshot, error) {
	var r RankSnapshot
	var best sql.NullFloat64
	err := database.QueryRow(`
		SELECT
			COALESCE(last_class, 'Unranked'),
			COALESCE(last_level, 0),
			COALESCE(last_percentile, 0),
			COALESCE(lifetime_wins, 0),
			COALESCE(lifetime_losses, 0),
			COALESCE(season_ordinal, 0),
			best_mythic_percentile
		FROM player_stats WHERE screen_name = ?`, screenName).
		Scan(&r.Class, &r.Level, &r.Percentile,
			&r.LifetimeWins, &r.LifetimeLosses, &r.SeasonOrdinal, &best)
	if err != nil {
		return r, err
	}
	if best.Valid {
		r.BestMythicPct = best.Float64
		r.HasBestMythic = true
	}
	return r, nil
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
