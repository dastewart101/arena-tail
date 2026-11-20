# arena-tail — Code Review & Issues

A local, Linux-only Magic: The Gathering Arena tracker. It tails MTGA's
`Player.log`, parses completed matches into SQLite, and shows deck performance
and player status in a Fyne GUI.

This document records issues found during review. Findings were validated by
building the project and running the **actual parser** over real logs
(`test-logs/Player.log`, `test-logs/Player-prev.log`).

## Scope

The five data points that matter are all captured correctly today:

| Field | Status |
|---|---|
| Deck being played | ✅ captured |
| On the play / on the draw | ✅ captured (no NULLs observed) |
| Won / lost | ✅ captured |
| Player lifetime W/L | ✅ captured |
| Rank level (and class) | ✅ captured |

Everything below is about **robustness, deployment, and cleanup** around that
working core — not about the core being wrong.

---

## 1. Correctness bugs

### 1.1 Scan→tail race silently drops matches
`cmd/arena-tail/main.go`

The resume offset for the live tailer is captured **after** the initial scan
finishes:

```go
scanFile(currentLog, ...)                 // reads to EOF at time T1
fi, _ := os.Stat(currentLog)              // size sampled at time T2
resumeOffset = fi.Size()                  // includes lines written T1..T2
StartTailerAt(currentLog, resumeOffset, ...) // starts reading *after* them
```

Any line written between the scan reaching EOF (T1) and the `os.Stat` (T2) is
counted in the offset but never parsed. A match completing in that window is
**lost**. The window is small but real, and MTGA writes heavily.

**Fix:** sample the size *before* scanning and tail from there. `match_id`
dedupe makes re-parsing the overlap harmless:

```go
var scanStart int64
if fi, err := os.Stat(currentLog); err == nil {
    scanStart = fi.Size()
}
// ... scanFile(currentLog, ...) ...
// ... tail from scanStart, not a post-scan Stat ...
```

### 1.2 `ParseLine` is a mutually-exclusive `switch` on substrings
`internal/parser/parser.go`

```go
switch {
case strings.Contains(line, "EventSetDeckV3"): ...
case strings.Contains(line, "finalMatchResult"), strings.Contains(line, "MatchCompletedReasonType"): ...
case strings.Contains(line, "gameRoomConfig"): ...
case strings.Contains(line, "turnInfo"): ...
case strings.Contains(line, "constructedClass"): ...
}
```

Only the **first** matching case runs. This works today by luck:
- A `finalMatchResult` line *also* contains `gameRoomConfig`; it only parses
  correctly because `finalMatchResult` is checked first.
- `turnInfo` and `gameRoomConfig` never co-occur (verified: 0 lines in both
  logs), so the `turnInfo` branch is reachable.

Reordering the cases — or any future marker that co-occurs — silently breaks
parsing with no error. Use `if / else if` with an explicit comment about
ordering, or parse independent markers independently.

### 1.3 A completed match is discarded if seats weren't seen
`internal/parser/parser.go` (`parseMatchResult`)

```go
if !found || !am.haveSeats {
    log.Printf("[PARSER] skipping %s ...", result.MatchID)
    delete(p.matches, result.MatchID)
    return
}
```

If the app starts mid-match, or a config line is missed across log rotation,
the entire match is thrown away. For a tracker, a partial row (`won = NULL`)
beats a missing row. At minimum, the skip is only logged to stdout — invisible
in a GUI app (see 3.2).

### 1.4 Deck stats split when a deck is renamed
`internal/db/matches.go` (`DecksPlayed`)

The query groups by `deck_id, deck_name`:

```sql
GROUP BY COALESCE(deck_id, ''), COALESCE(deck_name, 'Unknown Deck')
```

But `matches.deck_name` is frozen at match time. Rename a deck in Arena and its
history splits into two rows — one per name. Group by `deck_id` only and resolve
the display name from the `decks` table (which *is* upserted on rename).

### 1.5 Schema "migration" destroys non-rebuildable history
`internal/db/db.go` (`dropStaleTables`)

If any required column is missing, the whole table is **dropped**:

```go
if stale {
    log.Printf("[DB] dropping outdated %q table; it will be rebuilt from the logs", c.table)
    database.Exec(fmt.Sprintf("DROP TABLE %s", c.table))
}
```

The comment claims "the database is a cache rebuilt from the logs." That is
false for anything older than the current/previous log — **log rotation deletes
history permanently.** A column change would silently wipe lifetime match
history. Use real migrations (`ALTER TABLE ADD COLUMN`) or SQLite
`PRAGMA user_version` versioning.

### 1.6 Tailer error leaks a blocked goroutine
`internal/logger/tailer.go` + `cmd/arena-tail/main.go`

If `StartTailerAt` returns an error it never closes `lines`, so the consumer in
`main` blocks forever:

```go
for line := range lines { ... }   // never terminates if tailer died
```

Close the channel (e.g. `defer close(linesChan)`) when the tailer exits.

---

## 2. Dead code / unused surface (verified with grep)

| Location | Problem |
|---|---|
| `internal/ai/openrouter.go` | 1-line package decl, never imported. Implement or delete. |
| `internal/ui/app.go` | Entire `App` / `NewApp` / `RefreshData` unreferenced; `main.go` uses `views.NewDashboardView` directly. Delete. |
| `internal/db/matches.go` | `LifetimeTotals`, `Posture` (+ `Totals`, `PostureTotals`) never called. |
| `internal/ui/components/widgets.go` | `StatCard` struct + `Render()` dead; only free func `StatCardObjects` is used. |
| `internal/logger/tailer.go` | `StartTailer` (non-`At` wrapper) never called. |
| `internal/db/schema.sql` | `player_stats.best_mythic_rank` never read or written. |
| `internal/parser/events.go` | `rankPayload.ConstructedLeaderboard`, `ConstructedMatchesDrawn` parsed, never used. |

---

## 3. UI / UX issues

### 3.1 Fixed 620×320 window with no scrolling
`cmd/arena-tail/main.go` + `internal/ui/views/dashboard.go`

The deck list renders up to 5 decks × 2 rows, plus two stat cards, a header and
separators — far taller than 320 px. Content is clipped with no way to scroll.
Wrap the deck list in `container.NewVScroll` and/or grow the default window.

### 3.2 All errors are invisible
Every failure (`[PARSER]`, `[UI]`, `[DB]`, `[CONFIG]`) goes to `log`/`stdout`.
Launched from a `.desktop` file there is no terminal, so a silent "log dir not
found" or "skipping match" is undebuggable. Surface a status line / warning
banner in the window.

### 3.3 No controls
No reset/clear, no manual log-path override (`ARENA_PLAYER` is the only
override), no re-scan button, no settings. A tracker should let the user
re-point it at a log directory and clear history.

### 3.4 Direct `.Objects` mutation on refresh
`internal/ui/views/dashboard.go`

```go
v.rankCard.Objects = v.rankCardObjects()
v.rankCard.Refresh()
```

Reassigning `.Objects` directly bypasses Fyne's layout-invalidation conventions.
Rebuild the container and call `container.Refresh(c)`. (It runs on the main
thread via `fyne.Do`, so it is not a data race — just fragile.)

---

## 4. Architecture / hygiene

### 4.1 Zero tests
No `_test.go` files exist. The parser is pure string→JSON→row logic and is ideal
for table-driven tests. Sample lines from the real logs would lock in
deck/play/win/rank parsing and catch regressions like 1.2.

### 4.2 Substring markers rely on Go's case-insensitive JSON matching
`internal/parser/events.go` tags `matchId`, but the log actually writes
`matchID`. It works only because `encoding/json` matches field names
case-insensitively. An MTGA field rename would break parsing silently. Consider
matching the real casing and adding a marker/version guard.

### 4.3 Large artifacts committed to the working tree
`arena-tail` (36 MB binary), `arena_tail.db`, `test-logs/` (21 MB of logs) all
sit in the project root. Add a `.gitignore` (this repo has none) and keep build
output out of source control.

---

## 5. Deploying as a standalone app

Today the app assumes it is run from the project directory: the DB path is the
relative string `"arena_tail.db"` (`config.DefaultDBPath`) and all output goes
to stdout. For a distributable Linux app this needs to change.

### 5.1 Where to put the database

Do **not** use a CWD-relative path — it breaks when launched from a `.desktop`
file, where the working directory is `/`. Use the XDG base directories:

```go
// internal/config/config.go
func DefaultDBPath() (string, error) {
    dir, err := os.UserCacheDir()          // ~/.cache/arena-tail
    if err != nil {
        return "", err
    }
    dir = filepath.Join(dir, "arena-tail")
    if err := os.MkdirAll(dir, 0o700); err != nil {
        return "", err
    }
    return filepath.Join(dir, "arena_tail.db"), nil
}
```

- **Cache** (`~/.cache/arena-tail/`): the DB is rebuildable from logs, so cache
  is the natural home. Matches the current "it's a cache" intent.
- **Config** (`~/.config/arena-tail/`): if you add settings later
  (`config.toml`), put them here — not in the cache.
- Set `0700` on the dir and let SQLite keep `0600` on the file.

If you keep the DB in the cache, add a **Reset / Clear data** action in the UI
(see 3.3) so users can recover from a bad migration without hand-deleting files.

### 5.2 Separate logging for debugging

Right now there is no log file at all — `log.Printf` and `fmt.Printf` both go to
stdout, which is discarded when the app is launched from a desktop entry. Add a
file logger:

```go
// internal/logger/file.go
func SetupLogging() (func(), error) {
    dir, err := os.UserCacheDir()          // ~/.cache/arena-tail/
    if err != nil {
        return func() {}, err
    }
    dir = filepath.Join(dir, "arena-tail")
    if err := os.MkdirAll(dir, 0o700); err != nil {
        return func() {}, err
    }
    f, err := os.OpenFile(
        filepath.Join(dir, "arena-tail.log"),
        os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
    if err != nil {
        return func() {}, err
    }
    log.SetOutput(f)                       // route stdlib log to the file
    log.SetFlags(log.LstdFlags | log.Lmsgprefix)
    return func() { f.Close() }, nil
}
```

Recommendations:

- **Location:** `~/.cache/arena-tail/arena-tail.log` (alongside the DB). If the
  user must email it for support, `~/.local/state/arena-tail/` (XDG state) is
  the more correct location — cache may be cleared by cleaners.
- **Rotation:** cap it. Append-only with no rotation will grow unbounded. Roll
  at ~5 MB keeping one previous file, or use `lumberjack`.
- **Levels:** add a `--verbose` flag / env var (`ARENA_DEBUG=1`) and gate the
  noisy `[PARSER]` per-line diagnostics behind it, keeping warnings/errors
  always on.
- **Panics:** install `debug.SetCrashOutput` / a `recover` in `main` so a GUI
  crash writes a stack trace to the same file.
- **Still show something in the UI:** a file log does not replace 3.2. Write to
  both the file and a status line in the window.

### 5.3 Packaging

- **Binary:** build with `CGO_ENABLED=1` (required by `go-sqlite3` and Fyne) and
  `-ldflags="-s -w"` to shrink the 36 MB binary. It will not be a static
  single-file binary because of cgo + OpenGL; document the runtime deps
  (`libgl`, `libx11`, `libxcursor`, etc. — Fyne's Linux requirements).
- **Desktop entry:** ship `arena-tail.desktop` with an absolute `Exec=` and an
  icon. Ensure the entry does **not** rely on a working directory.
- **Log discovery is fragile:** `FindLogDir` hardcodes the Steam app id
  `2141910` and two Proton layouts. Add a settings override (3.3) and/or scan
  `compatdata/*/pfx/...` rather than one fixed app id, so non-Steam/Proton and
  native installs keep working.
- **First run:** if no log dir is found, show a clear dialog with instructions
  instead of silently tailing nothing.

### 5.4 Suggested minimal change set for standalone

1. `config.DefaultDBPath()` → `~/.cache/arena-tail/arena_tail.db` (5.1).
2. `logger.SetupLogging()` called at the top of `main`, with a `--verbose` flag
   (5.2).
3. Add `container.NewVScroll` + a status/warning label to the dashboard
   (3.1, 3.2).
4. Add `.gitignore` for `arena-tail`, `arena_tail.db`, `test-logs/`, `tmp/`
   (4.3).
5. Delete the dead packages/files in section 2.

---

## 6. Prioritized summary

**High (data loss / correctness)**
- 1.1 scan→tail race drops matches
- 1.5 drop-table migration wipes history
- 1.3 completed match discarded when seats unknown

**Medium (fragility / usability)**
- 1.2 `switch` marker ordering
- 1.4 deck rename splits stats
- 1.6 tailer error blocks goroutine
- 3.1 no scrolling / clipped UI
- 3.2 invisible errors
- 5.1 DB location for standalone
- 5.2 file logging for debugging

**Low (cleanup)**
- Section 2 dead code
- 3.3 no controls
- 3.4 `.Objects` mutation
- 4.1 no tests
- 4.2 case-insensitive JSON reliance
- 4.3 committed artifacts
