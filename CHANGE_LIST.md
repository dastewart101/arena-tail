# arena-tail — Change List (Verified Correctness, Data-Safety & Deployment)

Source: the "The two reviews are accurate — I verified the claims" section of
`ISSUES.md` / `recommend.md`. Every item below was re-checked against the actual
source in this repo (line numbers current as of this writing). This file is
self-contained so a fresh thread can start here without the prior chat.

Scope: correctness, data loss, resource-safety, deployment, and the front-end
UI gaps below (items 9-11). Remaining UI polish, dead-code removal, and tests are
tracked separately (see "Out of scope" at end).

---

## Priority order (do top to bottom)

| # | Item | Severity | Files |
|---|------|----------|-------|
| 1 | Drop-table migration wipes history | **Critical** | `internal/db/db.go` |
| 2 | Scan→tail race drops matches | **High** | `cmd/arena-tail/main.go` |
| 3 | Completed match discarded when seats unknown | **High** | `internal/parser/parser.go` |
| 4 | Tailer error leaks a blocked goroutine | **Medium** | `internal/logger/tailer.go`, `cmd/arena-tail/main.go` |
| 5 | Deck rename splits per-deck stats | **Medium** | `internal/db/matches.go` |
| 6 | `ParseLine` mutually-exclusive `switch` | **Medium** | `internal/parser/parser.go` |
| 7 | DB path is CWD-relative (breaks from desktop) | **High** | `internal/config/config.go`, `cmd/arena-tail/main.go` |
| 8 | No file logging; stdout discarded; no retention | **Medium** | new `internal/logger/file.go`, `cmd/arena-tail/main.go` |
| 9 | Deck list overflows fixed window; no scroll; new decks unreachable | **High (UI)** | `internal/ui/views/dashboard.go`, `cmd/arena-tail/main.go` |
| 10 | No way to delete a deck (DB + UI) | **Medium** | `internal/db/matches.go`, `internal/ui/views/dashboard.go` |
| 11 | Rank card: split season % vs lifetime best; add lifetime High rank | **Medium** | `internal/ui/views/dashboard.go`, `internal/db/matches.go`, `internal/db/schema.sql`, `internal/parser/events.go` |

---

## 1. Drop-table migration wipes non-rebuildable history — CRITICAL

**File:** `internal/db/db.go`
**Functions:** `InitDB` (line 31), `dropStaleTables` (lines 45–93),
`tableMissingColumn` (lines 98+)

**Problem:** `dropStaleTables` runs `DROP TABLE` whenever a required column is
missing (line 63, and the legacy sweep at line 89). The comment claims the DB is
a cache "rebuilt from the logs," but that is **false** for anything older than
`Player.log` + `Player-prev.log`: log rotation deletes that history permanently.
A future column change would silently wipe lifetime match history.

**Change:**
1. Replace `DROP TABLE` with additive migrations.
2. Introduce schema versioning via `PRAGMA user_version`:
   - Read current `user_version` at startup.
   - Apply ordered, idempotent migration steps (`ALTER TABLE ... ADD COLUMN`,
     data backfills) to move from the stored version to the target version.
   - Update `user_version` after each successful step.
3. Keep `CREATE TABLE IF NOT EXISTS` for fresh installs.
4. Remove `dropStaleTables` / `tableMissingColumn` / the `%_legacy_%` sweep once
   migrations exist, OR keep a narrowly-scoped version that only drops
   genuinely disposable caches — never `matches` / `player_stats` / `decks`.

**Migration steps to add now (baseline):**
- `v0 → v1`: ensure `matches.player_name` exists (add column if missing).
- `v0 → v1`: ensure `player_stats.lifetime_wins`/`lifetime_losses` exist.
- `v0 → v1`: ensure `decks.deck_id` exists.

**Acceptance:** Starting the app against a DB with an older column set must
**preserve existing rows**, not drop them. Verify with a copy of the current
`arena_tail.db`.

---

## 2. Scan→tail race silently drops matches — HIGH

**File:** `cmd/arena-tail/main.go`
**Lines:** scan at 63, `os.Stat`/`resumeOffset` at 69–71, tail start at 77

**Problem:**
```
scanFile(currentLog, ...)          // reads to EOF at T1
fi, _ := os.Stat(currentLog)       // size sampled at T2
resumeOffset = fi.Size()           // includes lines written T1..T2
StartTailerAt(currentLog, resumeOffset, ...)  // reads AFTER them -> lost
```
Any line written between scan-EOF (T1) and the `os.Stat` (T2) is counted in the
offset but never parsed. A match completing in that window is lost.

**Change:**
1. Sample the size **before** scanning:
   ```go
   var scanStart int64
   if fi, err := os.Stat(currentLog); err == nil {
       scanStart = fi.Size()
   }
   ```
2. Run `scanFile(currentLog, ...)`.
3. Tail from `scanStart`, **not** a post-scan `os.Stat`.
4. Rely on existing `match_id` dedupe (INSERT ... ON CONFLICT DO NOTHING) so the
   re-parsed overlap is harmless.

**Acceptance:** Overlap region (lines between `scanStart` and EOF) is parsed
exactly once into the DB; no match present in the log is missing from `matches`.

---

## 3. Completed match discarded when seats weren't seen — HIGH

**File:** `internal/parser/parser.go`
**Function:** `parseMatchResult`; guard at lines ~249–253 (`skipping` / `delete`)

**Problem:** If the app starts mid-match, or a config line is missed across log
rotation, the whole match is thrown away:
```go
if !found || !am.haveSeats {
    log.Printf("[PARSER] skipping %s ...", result.MatchID)
    delete(p.matches, result.MatchID)
    return
}
```
For a tracker, a partial row (`won = NULL` / unknown posture) beats a missing
row.

**Change:**
1. Always insert a row when `found` (a `MatchScope_Match` result exists).
2. When `haveSeats` is false, persist with nullable/unknown fields:
   - `won` unknown (NULL) or best-effort, `on_play` NULL, `my_team_id` NULL.
   - Keep `rank_class`/`rank_level` from `p.lastRank` as today.
3. Only fully skip when there is **no** match-scope result (`!found`).
4. Keep the diagnostic log, but see the UI status-line item (separate list) so
   it is visible outside a terminal.

**Schema note:** confirm `matches.won` / `on_play` / `my_team_id` permit NULL
(today `Won bool` is written as int; adjust `MatchRecord`/insert path to allow
"unknown").

**Acceptance:** A match with a result line but no preceding seat config produces
a row (partial), not a deletion.

---

## 4. Tailer error leaks a blocked goroutine — MEDIUM

**Files:** `internal/logger/tailer.go` (lines 13, 20, 49, 53, 63),
`cmd/arena-tail/main.go` (lines 77, 83)

**Problem:** `StartTailerAt` never closes the channel it was handed. If it
returns (error or file re-open failure), the consumer:
```go
for line := range lines { ... }   // main.go:83
```
blocks forever. The app wedges with no error surfaced.

**Change:**
1. In the goroutine that calls `StartTailerAt` (main.go:77), `defer close(lines)`
   so the consumer loop terminates.
2. Ensure `StartTailerAt` returns on all fatal paths (it does) so the deferred
   close fires.
3. On the consumer side, after the loop ends, log that live tailing stopped.
4. Also delete the dead `StartTailer` wrapper (line 13) — tracked in the
   dead-code list, but note it here because it shares this file.

**Acceptance:** Killing/renaming the log mid-run causes the app to stop tailing
cleanly and log it, rather than hanging.

---

## 5. Deck rename splits per-deck stats — MEDIUM

**File:** `internal/db/matches.go`
**Function:** `DecksPlayed` (line 181); `GROUP BY` at line 194

**Problem:** Grouping by both id and name:
```sql
GROUP BY COALESCE(deck_id, ''), COALESCE(deck_name, 'Unknown Deck')
```
`matches.deck_name` is frozen at match time, so renaming a deck in Arena splits
its history into two rows (one per name).

**Change:**
1. Group by `COALESCE(deck_id, '')` **only**.
2. Resolve the display name from the `decks` table (which *is* upserted on
   rename) via `LEFT JOIN decks d ON d.deck_id = matches.deck_id`, using the
   stored `deck_name` only as a fallback when the join misses.
3. Keep the `'Unknown Deck'` placeholder for `deck_id` = ''.

**Acceptance:** After a rename in Arena, the deck shows as a single row with its
full history under the new name.

---

## 6. `ParseLine` is a mutually-exclusive `switch` on substrings — MEDIUM

**File:** `internal/parser/parser.go`
**Function:** `ParseLine`; `switch` at line 83, cases 84–92

**Problem:** Only the first matching case runs. It works today by luck:
- A `finalMatchResult` line **also contains `gameRoomConfig`**; it parses
  correctly only because `finalMatchResult` is listed first (line 86 before 88).
- `turnInfo` (90) and `gameRoomConfig` (88) never co-occur today, so the
  `turnInfo` branch is reachable — again by luck.
Reordering cases, or any future co-occurring marker, silently breaks parsing.

**Change (pick one, but do it deliberately):**
- **Option A (minimal):** convert to `if / else if` and add an explicit comment
  that the order is load-bearing.
- **Option B (preferred):** make independent markers independent — e.g. handle
  `finalMatchResult` and `gameRoomConfig` in the same branch (they co-occur), and
  gate each handler on its own marker so ordering no longer matters.
- This pairs with the parser perf work in `recommend.md` section 4 (marker-gated
  `[]byte` state machine). If that perf rewrite happens, it must preserve this
  ordering guarantee or remove the dependency — call it out in the PR.

**Acceptance:** Table-driven test (see fixtures in `recommend.md` section 5)
passes, and reordering branches does **not** change results.

---

## 7. DB path is CWD-relative — breaks when launched from the desktop — HIGH

**Files:** `internal/config/config.go` (`DefaultDBPath = "arena_tail.db"`),
`cmd/arena-tail/main.go` (`db.InitDB(config.DefaultDBPath)`)

**Problem:** `config.DefaultDBPath` is the bare relative string
`"arena_tail.db"`. When the app is launched from a `.desktop` entry, the working
directory is typically `/` (or `$HOME`), not the project dir — so the DB is
created at the wrong path, or fails to open (read-only `/`), or a *second* DB is
silently created in a different location than the one the developer expects.
The current `arena_tail.db` lives in the project root only because it has always
been run from that directory.

**Decision — where the DB goes (XDG):**

| Data | Location | Env var | Rationale |
|------|----------|---------|-----------|
| SQLite DB | `~/.cache/arena-tail/arena_tail.db` | `$XDG_CACHE_HOME` | Rebuildable-from-logs cache; matches current intent. |
| Future settings | `~/.config/arena-tail/config.toml` | `$XDG_CONFIG_HOME` | User config belongs in config, never cache. |
| Debug logs | `~/.local/state/arena-tail/` | `$XDG_STATE_HOME` | Survives cache cleaners; correct home for logs. |

Rationale: the SQLite DB is a **cache** today (the reviews treat it as
rebuildable), so `~/.cache` is the natural home. BUT note the
tension with item 1: the moment we stop treating it as disposable (real
migrations, keep history), it stops being a pure cache and arguably belongs in
`~/.local/share/arena-tail/` (XDG data) instead. **Pick one and be consistent;
recommended: `~/.local/share/arena-tail/arena_tail.db`** so lifetime history is
not subject to cache eviction, with `~/.cache` reserved for throwaway files.

**Change:**
1. Replace the `DefaultDBPath` const with a function:
   ```go
   func DefaultDBPath() (string, error) {
       dir, err := os.UserCacheDir() // or os.UserConfigDir / XDG data
       if err != nil { return "", err }
       dir = filepath.Join(dir, "arena-tail")
       if err := os.MkdirAll(dir, 0o700); err != nil { return "", err }
       return filepath.Join(dir, "arena_tail.db"), nil
   }
   ```
2. `main.go` handles the error and exits with a logged message (see item 8).
3. Add a **`--db` flag / env override** (`ARENA_DB`) for testing and power users.
4. On first run with the new path, **migrate** an existing CWD `arena_tail.db`
   if present (copy once) so users do not lose history on upgrade — or document
   the move. Do not silently ignore an existing DB.
5. Directory perms `0700`; let SQLite keep `0600` on the file.

**Acceptance:** Launch from a clean `.desktop` entry (CWD `/`) creates/opens the
DB under the XDG path, not `/`. App finds existing history on relaunch.

---

## 8. No file logging; errors vanish; logs never clean up — MEDIUM

**Files:** new `internal/logger/file.go`; `cmd/arena-tail/main.go` (call it
first thing in `main`); every `log.Printf` / `fmt.Printf` site is affected.

**Problem:** All output goes to `log`/`stdout`. Launched from a `.desktop`
entry there is no terminal, so `[PARSER] skipping ...`, `[CONFIG] log dir not
found`, `[DB] dropping ...`, and `[UI]` errors are **invisible and undebuggable**.
There is also currently no retention story: any log file added naively would grow
unbounded.

**Decision — where debug logs go and how long they live:**

| Item | Value |
|------|-------|
| Directory | `~/.local/state/arena-tail/` (`$XDG_STATE_HOME`, fallback `~/.local/state`) |
| Active file | `arena-tail.log` |
| Rotation | Roll at ~5 MB, keep 1 previous file (`arena-tail.log.1`) |
| Retention (age) | Delete files older than **14 days** on startup |
| Retention (count) | Never keep more than **3** log files total |
| Max on-disk footprint | Hard cap; prune oldest first if the dir exceeds **~25 MB** |
| Perms | Dir `0700`, file `0600` |

Why `~/.local/state` and not `~/.cache`: cache may be cleared by system cleaners
at any time, which is fine for the DB if it is disposable — but debug logs you
might need *after* a crash should survive a cleaner. `state` is the XDG-correct
home for logs. (If the DB moves to `~/.local/share` per item 7, logs still belong
in `state`.)

**Change:**
1. Add `internal/logger/file.go`:
   ```go
   func SetupLogging(verbose bool) (cleanup func(), err error) {
       dir := stateDir("arena-tail")        // ~/.local/state/arena-tail, 0700
       pruneOldLogs(dir, 14*24*time.Hour, 3, 25<<20) // age, count, bytes
       f, err := os.OpenFile(filepath.Join(dir, "arena-tail.log"),
           os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
       if err != nil { return func(){}, err }
       log.SetOutput(io.MultiWriter(f)) // optionally os.Stderr when verbose
       log.SetFlags(log.LstdFlags | log.Lmsgprefix)
       return func(){ f.Close() }, nil
   }
   ```
2. Call it as the **first** line of `main`; `defer cleanup()`.
3. **Rotation:** either cap size inline (check file size on write) or use
   `gopkg.in/natefinch/lumberjack.v2` (simplest, battle-tested; adds one dep).
   Recommended: lumberjack with `MaxSize=5`, `MaxBackups=1`, `MaxAge=14`,
   `Compress=true` — it covers rotation + retention in one dependency.
4. **Prune on startup** for the cases lumberjack does not cover (stray files,
   multiple months of old logs): delete `.log`/`.log.gz` older than the age
   limit, then enforce the count and total-bytes caps, deleting oldest first.
5. Add a `--verbose` flag / `ARENA_DEBUG=1` env: when set, also mirror to
   stderr (and/or raise level). Gate the noisy `[PARSER]` per-line diagnostics
   behind it; keep warnings/errors always on.
6. **Panics:** `defer recover()` in `main` (or `debug.SetCrashOutput`) writes a
   stack trace to the same file so a GUI crash is diagnosable.
7. Combine the retention rule in **one** function (`pruneOldLogs`) so age,
   count, and byte-cap logic live together and are unit-testable.
8. File logging does **not** replace an in-window status line (see UI list):
   still surface "log dir not found" / "tailer stopped" to the user.

**Security note:** logs can contain the player's screen name and deck names.
Keep perms `0600`; do not log full log file contents.

**Acceptance:**
- Running from a `.desktop` entry writes `~/.local/state/arena-tail/arena-tail.log`.
- After exceeding 5 MB the log is rolled, not truncated in place.
- A pre-seeded directory containing logs older than 14 days is pruned on
  startup, and the directory never exceeds the byte cap.

---

### 9. Deck list overflows the fixed window — new decks are unreachable — HIGH (UI) — ✅ DONE
**Status:** Done (Option A — scrollable deck table). One acceptance line still at risk; see "Open issue".
**Files:** `internal/ui/views/dashboard.go` (deck section reworked; `deckListObjects`,
`deckList` field removed), `cmd/arena-tail/main.go` (window sizing).
**Problem:** The deck list was an unconstrained `container.NewVBox(...)` in the
root `VBox`, with `DecksPlayed` called with `limit = 0` (no cap). The window was
fixed at **620x320** with no scroll container, so rows past the window height
were laid out below the visible area and were unreachable — no scrollbar, no
resize. Newly played decks were exactly the ones that fell off the bottom.
**What was implemented (Option A):**
1. Replaced the VBox of labels with a `widget.Table`. Column spec is centralised
in a `deckColumns` var so widths and header titles cannot drift:
Deck 440 / W-L 90 / Win % 80 / On Play 110 / On Draw 110 (**830px total**).
2. `deckListObjects()` and the `deckList` field were deleted. `Refresh()` no
longer mut

**Change (pick one layout; A is recommended):**

- **Option A — scrollable deck table (preferred).** Replace the ad-hoc
  `VBox` of labels with a `widget.Table` (or `widget.List`) inside a
  `container.NewVScroll(...)`:
  1. Promote `deckListObjects()` to feed a table whose columns are the values
     already shown today: Deck, W-L, Win %, On-Play W-L (%), On-Draw W-L (%).
  2. Wrap the table in `container.NewVScroll` so the header row and the rank /
     lifetime cards stay pinned while the deck rows scroll.
  3. Sort so the most recently played / most-played deck is visible first (the
     SQL already orders by `COUNT(*) DESC`; add `MAX(ended_at) DESC` as a
     tiebreak so a brand-new deck surfaces immediately).
  4. This also retires the `.Objects` in-place mutation in `Refresh()`
     (lines 58-71) for the deck area — hand the table a new data slice and call
     `table.Refresh()`, which is the fyne-idiomatic path.
- **Option B — resizable window + scroll.** Keep the label layout but set a
  larger default size and mark the window resizable, still wrapping the deck
  area in `NewVScroll`. Minimum viable, but a table scales better.

**Also required regardless of option:**
- Make the window resizable and raise the default height (e.g. 620x520) in
  `main.go:61`; the fixed 320px height is the root cause.
- Keep a sane `DecksPlayed` limit for first paint if the history is huge (e.g.
  top 50) with a "show all" affordance, so the app does not try to render
  thousands of rows at startup.

**Acceptance:**
- Playing a new deck makes it appear in the visible list without manual resizing.
- With 30+ decks in the DB, every deck is reachable by scrolling, and the rest
  of the dashboard stays visible.
- Window can be resized and the deck list grows with it.

---

## 10. No way to delete a deck (DB + UI) — MEDIUM

**Files:** `internal/db/matches.go` (near `DecksPlayed` line 181),
`internal/ui/views/dashboard.go` (`deckListObjects` line 131,
`BuildUI` line 158)

**Problem:** Decks are only ever upserted (`db.UpsertDeck`, `matches.go`), never
removed. A mis-detected deck, a test/practice deck, or a deck the player wants to
forget stays in `decks` and therefore in the dashboard's "Deck Performance"
forever. There is no delete path in either the DB layer or the UI.

**Decision — what "delete" means (choose deliberately; recommended: A):**

- **Option A — delete matches for the deck, keep the row out of the list.**
  Delete the deck's match rows so its stats disappear from every aggregate, then
  delete the `decks` row. This is the intuitive "remove this deck" behavior, but
  it destroys match history (and it is *not* recoverable from logs once those
  rotate — see item 1).
- **Option B — soft-delete / hide.** Add `decks.hidden` (or `deleted_at`) and
  exclude hidden decks from `DecksPlayed`. Match history is preserved; the deck
  can be un-hidden later.
- Recommendation: implement **B** as the default action ("Hide this deck") and
  offer **A** behind an explicit, confirmed "Delete deck and its matches"
  (destructive) choice, so a stray click cannot wipe history.

**Change:**
1. **DB (`internal/db/matches.go`):**
   - For Option A: `DeleteDeck(database, deckID)` runs in one transaction —
     `DELETE FROM matches WHERE deck_id = ?` then
     `DELETE FROM decks WHERE deck_id = ?`. Return the number of rows removed so
     the UI can confirm.
   - For Option B: add a `hidden INTEGER NOT NULL DEFAULT 0` column via a new
     **migration step** (extend `migrations`/`schemaVersion` in
     `internal/db/db.go` per item 1 — do not hand-edit `arena_tail.db`), plus
     `SetDeckHidden(database, deckID, hidden bool)`. Update `DecksPlayed` to add
     `AND COALESCE(d.hidden, 0) = 0` (or filter on `deleted_at IS NULL`).
   - Either way, the `device_id`/`match_id`-keyed aggregates (`LifetimeTotals`,
     `Posture`) should exclude deleted/hidden decks consistently if that is the
     chosen semantic — state it explicitly in the PR.
2. **UI (`internal/ui/views/dashboard.go`):**
   - Add a per-deck action: a small delete/hide button in the row (feasible once
     the table from item 9 exists — use a cell with a `widget.Button`), or a
     right-click/long-press action.
   - Show a confirmation dialog (`widget.NewConfirm`) naming the deck and stating
     clearly whether match history will be deleted or merely hidden.
   - On confirm, call the DB function, then `Refresh()` the dashboard so the
     aggregate cards update too.
   - After item 9's table lands, the delete column is the natural home for this;
     before then, a single "Manage decks" list is acceptable.
3. **Safety:** the destructive variant must be explicit (typed/y/N confirm) and
   must log what was removed (ties into item 8's file logging).

**Acceptance:**
- A deck can be removed from the dashboard without hand-editing the DB.
- For Option A: after deletion, the deck no longer appears and its matches are
  gone from all aggregates; the action is confirmed first.
- For Option B: the deck disappears from the list but its history is intact and
  reversible.

---

## 11. Rank card: split season % from lifetime best; add lifetime High rank — MEDIUM

**Files:** `internal/ui/views/dashboard.go` (`rankValue` line 99,
`rankSub` line 106, `rankCardObjects` line 118),
`internal/db/matches.go` (`RecordRank`, `Rank`, `RankSnapshot`),
`internal/db/schema.sql` (`player_stats`), `internal/parser/events.go`
(`rankPayload`)

**Problem:** The "Rank Status" card currently mashes the *current/season* rank
and the *lifetime best* together on one line (`rankSub`, line 106):
`"Season %d / Level %d"` plus `"/ Best %.1f%%"` only when the best beats the
current percentile. Two problems:
1. **Season vs lifetime is conflated.** `constructedPercentile` (season /
   current) and `best_mythic_percentile` (lifetime best) are different things and
   the user asked for them **broken out as separate fields**.
2. **No lifetime High rank.** The best is stored as a *percentile only*
   (`best_mythic_percentile`). There is no record of the highest **rank/level**
   ever reached (e.g. "Mythic", or the highest constructed level), and
   `best_mythic_rank` exists in `schema.sql` but is written nowhere and shown
   nowhere (it is in the dead-code list today). The user wants a **lifetime High
   rank** field that populates once the player enters the Mythic ranking.

**Change:**

1. **Schema (`internal/db/schema.sql` + a migration step per item 1):** the
   `player_stats` table already has `best_mythic_percentile` and an unused
   `best_mythic_rank`. Decide the canonical column set for "lifetime high":
   - `best_rank_class TEXT` — best class ever seen (e.g. `"Mythic"`),
   - `best_rank_level INTEGER` — best constructed level ever seen,
   - `best_rank_percentile REAL` — best percentile ever seen (reuse
     `best_mythic_percentile` or rename for clarity),
   - optionally `best_season_ordinal INTEGER` — which season that high was set
     in, so the UI can label it.
   Add the missing columns via a new migration (`ALTER TABLE ... ADD COLUMN`),
   bumping `schemaVersion`. Do **not** reuse `best_mythic_rank` for a percentile;
   either repurpose it as `best_rank_level` or drop it from the dead list and add
   a clearly named column.

2. **Parser (`internal/parser/events.go`, `parser.go::parseRank`):** `rankPayload`
   already carries `constructedClass`, `constructedLevel`,
   `constructedPercentile`, `constructedSeasonOrdinal`. Source the lifetime-high
   fields from these same values on each rank line — i.e. "high rank" is derived
   from MTGA's own reported rank across the session/history, not a separate
   payload. (Confirm whether MTGA emits a dedicated highest-rank field; the
   sample payload here does not, so highest-seen is the correct source.)

3. **DB (`internal/db/matches.go::RecordRank`):** extend the `ON CONFLICT` upsert
   to track maxima with a rank-aware comparison, not just `MAX(percentile)`:
   - Class ordering: define a rank order (e.g. `Unranked < Bronze < Silver <
     Gold < Platinum < Diamond < Mythic`; confirm exact MTGA class strings) and
     store the best class seen.
   - Level: keep `MAX` within the best class (a higher level is better only at
     the same or better class).
   - Percentile: take the best percentile only when class/level are equal, since
     a *lower* Mythic percentile is better (top 1% < top 50%) — verify the
     direction of MTGA's percentile and encode it explicitly. This is subtle:
     `best_mythic_percentile` today uses raw `MAX`, which is only correct if
     MTGA reports "percentile = rank position where lower is better" inverted;
     confirm against a known value before trusting it.
   - Expose the new fields on `RankSnapshot` and select them in `Rank`.

4. **UI (`internal/ui/views/dashboard.go`):** split the card into clearly labeled
   fields:
   - **Current / season:** `rankValue()` shows class + season percentile for the
     *current* season (`ConstructedClass`, `Percentile`, `SeasonOrdinal`).
   - **Lifetime best:** a separate labeled line/field, e.g.
     `"Best: Mythic (top 3.2%, S12)"`, shown only once known.
   - **High rank:** a dedicated "Lifetime High" field that populates when the
     player reaches Mythic (i.e. `best_rank_class == "Mythic"` and
     `best_rank_level >= <Mythic base level>`), displaying the rank name and,
     when available, the percentile — otherwise show `"—"` / `"Not yet"`.
   - Keep `rankSub` for level/season context, but move the "Best" text out into
     its own field so season and lifetime are never conflated again.

**Acceptance:**
- The Rank card shows **current/season rank and percentage** and **lifetime best
  rank** as distinct, labeled fields.
- Once the player enters Mythic, a **Lifetime High** field populates and
  persists across restarts.
- Best-rank tracking is monotonic (it never regresses when the player drops) and
  the percentile direction is documented in code.

---

## Suggested commit sequence

1. **db migration** (item 1) - highest risk to data; do first, verify on a DB copy.
2. **scan->tail race** (item 2).
3. **keep partial matches** (item 3) - may need a small schema NULL check.
4. **tailer close** (item 4).
5. **deck rename grouping** (item 5).
6. **switch ordering** (item 6).
7. **DB path to XDG** (item 7) - include the one-time migration of an existing
   CWD `arena_tail.db` so upgrades do not lose history.
8. **file logging + retention** (item 8) - can land independently; do it early so
   items 1-7 are debuggable if run from a desktop entry.
9. **deck list scroll/table + resizable window** (item 9) - unblocks seeing new
   decks; do this before 10 so the delete action has a row to live in.
10. **deck delete/hide** (item 10) - needs the item 1 migration for the `hidden`
    column if the non-destructive Option B is chosen.
11. **rank card split + lifetime high rank** (item 11) - needs its own migration
    for the best-rank columns; land after item 1's framework exists.

Items 1, 3, 5, 7, 10, and 11 touch DB/persistence behavior - test each against a
copy of `arena_tail.db`, against a fresh DB, and (for 7) from a `.desktop` entry
with CWD `/`. Items 10 and 11 depend on the item 1 migration mechanism, so land
item 1 first.

---

## Out of scope (separate lists)


- **UI/UX (remaining, not in items 9-11):** status/warning line, additional controls (re-scan / clear / log-dir override). NOTE: scrollable deck Table (item 9), larger/resizable window (item 9), deck delete/hide (item 10), and the rank-card split (item 11) are now **in scope** above — remove them from this list.
- **Packaging (beyond items 7-8):** gitignore (binary, DB, test-logs), desktop entry with absolute Exec and icon, log-dir discovery hardening (the hardcoded Steam app id 2141910).
- **Tests:** zero _test.go files today; recommend.md section 5 has ready fixtures. pruneOldLogs (item 8) is a good first unit-test target.

---

## Verification baseline (already confirmed this session)

- Parser works end-to-end: replaying the live log into a fresh DB produced matches with deck / on-play / win / rank populated.
- Live tailer works: appended lines were received via StartTailerAt.
- The production arena_tail.db contains correctly-written rows (deck fields populated), confirming the write path - so these items are about robustness, not a broken core.
