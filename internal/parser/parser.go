package parser

import (
	"database/sql"
	"encoding/json"
	"log"
	"strconv"
	"strings"
	"time"

	"arena-tail/internal/config"
	"arena-tail/internal/db"
)

// activeMatch accumulates state until a result arrives for its match ID.
type activeMatch struct {
	mySeatID  int
	myTeamID  int
	onPlay    *bool
	deckID    string
	deckName  string
	haveSeats bool
}

// LogParser turns MTGA log lines into rows in the database. Markers are based
// on the payloads that actually appear in real logs: RankUpdated, DeckSubmit,
// onThePlay and deckName never occur and are deliberately not used.
type LogParser struct {
	db         *sql.DB
	screenName string
	sourceLog  string

	matches map[string]*activeMatch

	// Deck most recently selected for an event. The selection payload carries
	// no match ID, so it is kept at session scope and attached to later matches.
	curDeckID   string
	curDeckName string
	curFormat   string

	lastRank rankPayload
}

func NewLogParser(database *sql.DB, screenName, sourceLog string) *LogParser {
	name := config.NormalizePlayerName(screenName)
	if name == "" {
		name = config.FallbackPlayerName
	}
	p := &LogParser{
		db:         database,
		screenName: name,
		sourceLog:  sourceLog,
		matches:    make(map[string]*activeMatch),
	}
	if database != nil {
		_, err := database.Exec(
			`INSERT INTO player_stats (screen_name) VALUES (?) ON CONFLICT(screen_name) DO NOTHING`,
			name)
		if err != nil {
			log.Printf("[PARSER] could not seed player_stats: %v", err)
		}
		// Seed the last-known rank from the database so matches whose result
		// line precedes the next rank line still record a class instead of NULL.
		if r, err := db.Rank(database, name); err == nil && r.Class != "" {
			p.lastRank = rankPayload{
				ConstructedClass:         r.Class,
				ConstructedLevel:         r.Level,
				ConstructedPercentile:    r.Percentile,
				ConstructedMatchesWon:    r.LifetimeWins,
				ConstructedMatchesLost:   r.LifetimeLosses,
				ConstructedSeasonOrdinal: r.SeasonOrdinal,
			}
		}
	}
	return p
}

// PlayerName exposes the normalized name so callers never guess the key.
func (p *LogParser) PlayerName() string { return p.screenName }

// ParseLine dispatches on markers verified against the sample logs.
//
// Markers are handled independently rather than via a mutually-exclusive
// switch. A finalMatchResult line also contains gameRoomConfig, and turnInfo
// can in principle co-occur with either, so ordering must not decide which
// handler runs. Each handler is gated on its own marker.
func (p *LogParser) ParseLine(line string) {
	if strings.Contains(line, "EventSetDeckV3") {
		p.parseDeckSelection(line)
	}
	// A finalMatchResult line also carries gameRoomConfig (seats); parse both so
	// the result path can harvest seats even when no config line was seen.
	if strings.Contains(line, "finalMatchResult") || strings.Contains(line, "MatchCompletedReasonType") {
		p.parseMatchResult(line)
	} else if strings.Contains(line, "gameRoomConfig") {
		p.parseGameRoomConfig(line)
	}
	if strings.Contains(line, "turnInfo") {
		p.parseTurnInfo(line)
	}
	if strings.Contains(line, "constructedClass") {
		p.parseRank(line)
	}
}

// jsonBody returns the first JSON object in a prefixed log line, if any.
func jsonBody(line string) string {
	i := strings.IndexByte(line, '{')
	if i < 0 {
		return ""
	}
	return line[i:]
}

func (p *LogParser) parseDeckSelection(line string) {
	body := jsonBody(line)
	if body == "" {
		return
	}
	var env eventSetDeckEnvelope
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		return
	}
	var req eventSetDeckRequest
	if err := json.Unmarshal([]byte(env.Request), &req); err != nil {
		return
	}
	if req.Summary.DeckID == "" {
		return
	}

	p.curDeckID = req.Summary.DeckID
	p.curDeckName = req.Summary.Name
	for _, a := range req.Summary.Attributes {
		if a.Name == "Format" {
			p.curFormat = a.Value
		}
	}

	if p.db != nil {
		if err := db.UpsertDeck(p.db, db.Deck{
			DeckID: req.Summary.DeckID,
			Name:   req.Summary.Name,
			Format: p.curFormat,
		}); err != nil {
			log.Printf("[PARSER] %v", err)
		}
	}
}

// parseGameRoomConfig captures the player's seat and team, which are required
// to decide the result and whether they were on the play.
func (p *LogParser) parseGameRoomConfig(line string) {
	body := jsonBody(line)
	if body == "" {
		return
	}
	var env gameRoomEnvelope
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		return
	}
	cfg := env.MatchGameRoomStateChangedEvent.GameRoomInfo.GameRoomConfig
	if cfg.MatchID == "" {
		return
	}
	p.applyConfig(cfg.MatchID, cfg.ReservedPlayers)
}

func (p *LogParser) applyConfig(matchID string, players []struct {
	UserID       string `json:"userId"`
	PlayerName   string `json:"playerName"`
	SystemSeatID int    `json:"systemSeatId"`
	TeamID       int    `json:"teamId"`
}) {
	am := p.match(matchID)
	for _, rp := range players {
		if strings.EqualFold(config.NormalizePlayerName(rp.PlayerName), p.screenName) {
			am.mySeatID = rp.SystemSeatID
			am.myTeamID = rp.TeamID
			am.haveSeats = true
		}
	}
	p.attachDeck(am)
}

func (p *LogParser) attachDeck(am *activeMatch) {
	if am.deckID == "" && p.curDeckID != "" {
		am.deckID = p.curDeckID
		am.deckName = p.curDeckName
	}
}

// parseTurnInfo decides play/draw from the first turn's active player, the only
// reliable signal in these logs.
func (p *LogParser) parseTurnInfo(line string) {
	body := jsonBody(line)
	if body == "" {
		return
	}
	var env greEnvelope
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		return
	}
	for _, msg := range env.GreToClientEvent.GreToClientMessages {
		gs := msg.GameStateMessage
		if gs.TurnInfo.TurnNumber != 1 || gs.TurnInfo.ActivePlayer == 0 {
			continue
		}
		if gs.GameInfo.MatchID == "" {
			continue
		}
		am := p.match(gs.GameInfo.MatchID)
		if am.onPlay != nil || !am.haveSeats {
			continue
		}
		onPlay := gs.TurnInfo.ActivePlayer == am.mySeatID
		am.onPlay = &onPlay
	}
}

func (p *LogParser) parseMatchResult(line string) {
	body := jsonBody(line)
	if body == "" {
		return
	}
	var env gameRoomEnvelope
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		return
	}

	info := env.MatchGameRoomStateChangedEvent.GameRoomInfo
	result := info.FinalMatchResult
	if result.MatchID == "" {
		return
	}

	// Seat, team and deck are often on this same line, so harvest them in case
	// no config or turn message was parsed first.
	if len(info.GameRoomConfig.ReservedPlayers) > 0 {
		p.applyConfig(result.MatchID, info.GameRoomConfig.ReservedPlayers)
	}
	am := p.match(result.MatchID)
	p.attachDeck(am)

	winningTeam := 0
	found := false
	var scopes []string
	for _, r := range result.ResultList {
		scopes = append(scopes, r.Scope)
		if r.Scope == "MatchScope_Match" || strings.HasSuffix(r.Scope, "_Match") {
			winningTeam = r.WinningTeamID
			found = true
		}
	}
	// myTeamID is deliberately not required to be non-zero: MTGA numbers teams
	// 0 and 1, so a guard of ==0 discarded every valid match. haveSeats already
	// proves the player was found in the config.
	if !found {
		log.Printf("[PARSER] skipping %s: no match-scope result (scopes=%v) player=%q",
			result.MatchID, scopes, p.screenName)
		delete(p.matches, result.MatchID)
		return
	}
	if !am.haveSeats {
		log.Printf("[PARSER] partial match %s: no seat info, recording unknown result (scopes=%v) player=%q",
			result.MatchID, scopes, p.screenName)
	}

	rec := db.MatchRecord{
		MatchID:        result.MatchID,
		PlayerName:     p.screenName,
		DeckID:         am.deckID,
		DeckName:       am.deckName,
		OnPlay:         am.onPlay,
		RankClass:      p.lastRank.ConstructedClass,
		RankLevel:      p.lastRank.ConstructedLevel,
		RankPercentile: p.lastRank.ConstructedPercentile,
		EndedAt:        parseEpochMillis(env.Timestamp),
		SourceLog:      p.sourceLog,
	}
	// Result and team are only known when the player's seat was seen.
	if am.haveSeats {
		won := winningTeam == am.myTeamID
		myTeam := am.myTeamID
		winTeam := winningTeam
		rec.Won = &won
		rec.MyTeamID = &myTeam
		rec.WinningTeamID = &winTeam
	}

	if p.db != nil {
		inserted, err := db.InsertMatch(p.db, rec)
		if err != nil {
			log.Printf("[PARSER] %v", err)
		} else if !inserted {
			log.Printf("[PARSER] duplicate match %s ignored", rec.MatchID)
		}
	}
	delete(p.matches, result.MatchID)
}

// parseRank captures the current rank plus MTGA's own lifetime win/loss
// counters. The payload is emitted on the line carrying "constructedClass".
func (p *LogParser) parseRank(line string) {
	body := jsonBody(line)
	if body == "" {
		return
	}
	var rk rankPayload
	if err := json.Unmarshal([]byte(body), &rk); err != nil {
		return
	}
	if rk.ConstructedClass == "" {
		return
	}
	p.lastRank = rk

	if p.db != nil {
		if err := db.RecordRank(p.db, p.screenName,
			rk.ConstructedClass, rk.ConstructedLevel, rk.ConstructedPercentile,
			rk.ConstructedMatchesWon, rk.ConstructedMatchesLost,
			rk.ConstructedSeasonOrdinal); err != nil {
			log.Printf("[PARSER] %v", err)
		}
	}
}

func (p *LogParser) match(matchID string) *activeMatch {
	am, ok := p.matches[matchID]
	if !ok {
		am = &activeMatch{}
		p.matches[matchID] = am
	}
	return am
}

// parseEpochMillis converts MTGA's millisecond timestamp, returning the zero
// time when it is absent or malformed.
func parseEpochMillis(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	ms, err := strconv.ParseInt(s, 10, 64)
	if err != nil || ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}
