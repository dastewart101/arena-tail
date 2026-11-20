package parser

// eventSetDeckEnvelope is the outer JSON of an EventSetDeckV3 line. The request
// field is itself an escaped JSON document, so it is decoded twice.
type eventSetDeckEnvelope struct {
	Request string `json:"request"`
}

type eventSetDeckRequest struct {
	EventName string `json:"EventName"`
	Summary   struct {
		DeckID     string `json:"DeckId"`
		Name       string `json:"Name"`
		Attributes []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"Attributes"`
	} `json:"Summary"`
}

// gameRoomEnvelope covers both the config and result halves of a match event.
type gameRoomEnvelope struct {
	Timestamp                      string `json:"timestamp"` // epoch milliseconds
	MatchGameRoomStateChangedEvent struct {
		GameRoomInfo struct {
			GameRoomConfig struct {
				MatchID         string `json:"matchId"`
				ReservedPlayers []struct {
					UserID       string `json:"userId"`
					PlayerName   string `json:"playerName"`
					SystemSeatID int    `json:"systemSeatId"`
					TeamID       int    `json:"teamId"`
				} `json:"reservedPlayers"`
			} `json:"gameRoomConfig"`
			FinalMatchResult struct {
				MatchID    string `json:"matchId"`
				ResultList []struct {
					Scope         string `json:"scope"`
					Result        string `json:"result"`
					WinningTeamID int    `json:"winningTeamId"`
				} `json:"resultList"`
			} `json:"finalMatchResult"`
		} `json:"gameRoomInfo"`
	} `json:"matchGameRoomStateChangedEvent"`
}

// greEnvelope carries per-game state used to determine who took the first turn.
type greEnvelope struct {
	GreToClientEvent struct {
		GreToClientMessages []struct {
			GameStateMessage struct {
				GameInfo struct {
					MatchID string `json:"matchId"`
				} `json:"gameInfo"`
				TurnInfo struct {
					TurnNumber   int `json:"turnNumber"`
					ActivePlayer int `json:"activePlayer"`
				} `json:"turnInfo"`
			} `json:"gameStateMessage"`
		} `json:"greToClientMessages"`
	} `json:"greToClientEvent"`
}

// rankPayload is emitted at the root of combined-rank responses. The
// constructed matches won/lost counters are MTGA's own lifetime totals.
type rankPayload struct {
	ConstructedClass         string  `json:"constructedClass"`
	ConstructedLevel         int     `json:"constructedLevel"`
	ConstructedPercentile    float64 `json:"constructedPercentile"`
	ConstructedLeaderboard   int     `json:"constructedLeaderboardPlace"`
	ConstructedMatchesWon    int     `json:"constructedMatchesWon"`
	ConstructedMatchesLost   int     `json:"constructedMatchesLost"`
	ConstructedMatchesDrawn  int     `json:"constructedMatchesDrawn"`
	ConstructedSeasonOrdinal int     `json:"constructedSeasonOrdinal"`
}
