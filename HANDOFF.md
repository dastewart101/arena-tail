# arena-tail — Handoff for new thread

Goal: fix "live game not recorded." All 8 items in CHANGE_LIST.md are
implemented and the app builds and runs. One bug remains.

## Environment
- Project: /home/darryl/arena-tail (Go, Fyne GUI, SQLite, no git)
- Editor: VSCodium has a hard ~4GB RAM ceiling; when it reaches it the window
  goes non-responsive and pops the "keep waiting / close" dialog. This is the
  root cause of cancelled terminal commands and the agent appearing "stuck": the
  commands are not failing, the editor is frozen. Mitigation: keep total
  terminal + editor memory usage around 3–3.5GB (comfortably under the 4GB
  limit) so tool calls don't crash the editor. Run fewer/lighter terminals,
  close unneeded ones, avoid `tee`-ing large streams, and prefer short,
  surgical tool calls over big multi-command pipelines.
- Log dir (MTGA): /home/darryl/.local/share/Steam/steamapps/compatdata/2141910/pfx/drive_c/users/steamuser/AppData/LocalLow/Wizards Of The Coast/MTGA/
- Copies of logs are in ./test-logs/ (Player.log, Player-prev.log, arena-tail.log)
- DB: ~/.local/share/arena-tail/arena_tail.db (XDG data; item 7 done)
- App log: ~/.local/state/arena-tail/arena-tail.log (item 8 done, rotation+retention)
- Player screen name: "edgarallen" (from log auth line)

## What works
- Build compiles. App runs from project dir and from a .desktop-style launch.
- Existing history present in DB.
- FILE LOGGING WORKS: run with `ARENA_DEBUG=1 ./arena-tail 2>&1 | tee /tmp/at-run.log`
  produces `[LOGGER] logging to /home/darryl/.local/state/arena-tail/arena-tail.log`.
- INITIAL SCAN WORKS: on startup it scans Player-prev.log then Player.log and
  emits many `[PARSER] duplicate match <id> ignored` lines (i.e. it re-sees ~40
  matches already in the DB; dedupe via INSERT ... ON CONFLICT DO NOTHING works).
- Tailer starts: log shows
  `[LOGGER] Tailer actively monitoring: <Player.log> (offset 21006454)`

## The bug
After the tailer reports active at offset 21006454, the user played a full game
(deck "professor (2)") and NOTHING was recorded:
- No new row in matches.
- No `[PARSER] duplicate`, no `[PARSER] partial match`, no `[PARSER] skipping`,
  no insert error — i.e. zero parser output after tailer start.
Because a *successful* InsertMatch is silent, "no output" could mean either
(a) the tailer never delivered the new lines, or (b) lines were delivered but
none matched a parser marker. Need to disambiguate.

## Most likely causes (in order)
1. TAILER NOT DELIVERING NEW LINES. StartTailerAt uses github.com/nxadm/tail
   with Poll:true, ReOpen:true, Location:&tail.SeekInfo{Offset:21006454,
   Whence:io.SeekStart}. Under Wine/Proton on Linux, polling+seek may not pick
   up appends, or the file may have been rotated/recreated (MTGA writes
   Player.log then rotates to Player-prev.log on restart) so the tailed inode is
   stale. ReOpen should handle truncation but NOT a brand-new file at a new
   inode in all cases. SUSPECT #1.
2. MARKER MISMATCH. If delivered, does the live result line actually contain
   `finalMatchResult` or `MatchCompletedReasonType`? The initial scan found
   matches, so markers DO match historical lines — but confirm the *live* game's
   result line uses the same marker (grep the NEW lines appended after offset
   21006454).
3. OFFSET TOO LARGE / TOO SMALL. offset is a byte position captured by
   os.Stat BEFORE the scan (item 2 race fix). If that file was truncated or
   replaced between stat and tail, seeking to 21006454 could land at/past EOF of
   the CURRENT file and then polling a stale handle. Verify the byte at that
   offset is sane.
4. A successful insert that the user simply didn't see refresh in the UI. Low
   probability given "no row in DB", but re-check `SELECT count(*) FROM matches`
   before/after a game.

## Fastest diagnostic (do first, in a new thread)
Determine whether the tailer delivers ANY line after start:
- Option A: temporarily add a one-line log in the `for line := range t.Lines`
  loop in internal/logger/tailer.go, e.g.
  `log.Printf("[TAIL] %d bytes: %.80s", len(line.Text), line.Text)`
  Rebuild, run `ARENA_DEBUG=1 ./arena-tail`, play/observe. If nothing prints
  after the game, it's cause #1 (tailer). If lines print but no PARSER lines,
  it's cause #2 (marker/parse).
- Option B (no rebuild): after the game, `grep -c finalMatchResult` on the LIVE
  Player.log and compare to before; and `tail -c 200000 <Player.log> | grep
  finalMatchResult | tail -1 | cut -c1-200` to see the live marker.

## If cause #1 (tailer): fix direction
- The nxadm/tail poll+seek path is the suspect under Proton. Consider replacing
  the tail loop with a simple manual follower: remember an offset, `os.Open` the
  file each poll (1s), `Seek(offset)`, `Read` new bytes, split on '\n', update
  offset, and detect inode change (os.Stat inode/size shrink) to reset to 0 on
  rotation. This avoids nxadm/tail's Wine edge cases entirely and removes a dep.
- Keep `line.Text != ""` filtering and the channel send.
- On rotation (size < lastOffset or inode changed) reset offset to 0 and re-read
  from start (match_id dedupe makes re-read harmless).

## If cause #2 (marker): fix direction
- Confirm the exact substring of the live result line and extend the marker set
  in ParseLine (parser.go). Historical matches prove `finalMatchResult` exists in
  these logs, so more likely the *live* lines weren't delivered (cause #1).

## Key file map
- Live wiring / startup: cmd/arena-tail/main.go
  (SetupLogging first; DefaultDBPath()+AdoptLegacyDB; scan prev; stat size;
   scan current; StartTailerAt(currentLog, offset, lines); consume `for line range lines`)
- Tailer: internal/logger/tailer.go (StartTailerAt — SUSPECT)
- File logs+retention: internal/logger/file.go (works)
- Parser/markers: internal/parser/parser.go (ParseLine, parseMatchResult)
- Parser payload structs: internal/parser/events.go
- DB: internal/db/db.go (versioned migrations), matches.go (InsertMatch,
  pointer Won/MyTeamID/WinningTeamID, DecksPlayed LEFT JOIN decks), schema.sql
  (won nullable)
- Config/XDG/legacy DB adopt: internal/config/config.go
- UI: internal/ui/views/dashboard.go (refresh on isStatsEvent lines)

## State of the 8 change-list items (all edited, compiles)
1. db migrations (no more DROP TABLE) — done
2. scan->tail race (stat size before scan) — done (but see offset suspect above)
3. keep partial matches (Won/MyTeamID pointers, won nullable) — done
4. tailer closes channel (defer close(lines)) + post-loop log — done
5. deck rename grouping (GROUP BY deck_id, LEFT JOIN decks) — done
6. ParseLine marker-gated (not mutually exclusive switch) — done
7. DB path to XDG + AdoptLegacyDB — done
8. file logging + rotation + retention — done (verified working)

## Do NOT redo
- Don't re-run the original 8 items; they're in.
- VSCodium memory: keep sessions short, few big commands.
- The user must run terminal commands if the editor is unresponsive; agent
  commands get cancelled. Ask for pasted output.
