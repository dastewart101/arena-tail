# arena-tail — Parser Findings & Recommendations

Scope: the five data points tracked by the app (deck, on play/on draw, won/lost,
lifetime W/L, rank). Findings below were produced by instrumenting the real logs
`test-logs/Player.log` (14 MB, 20,540 lines) and `test-logs/Player-prev.log`
(34.6 MB, 49,591 lines) and by benchmarking the actual `ParseLine` code.

---

## 1. Where the five fields actually come from

Each field is produced by a tiny handful of lines:

| Field | Source marker | Lines (current) | Bytes (current) |
|---|---|---|---|
| Deck being played | `EventSetDeckV3` | 10 | 13.6 KB |
| On play / on draw | `DieRollResultsResp` (see §1.2) | 5 | ~77 KB |
| Won / lost | `finalMatchResult` | 5 | 5.9 KB |
| Player seat / team | `gameRoomConfig` | 10 | 11.0 KB |
| Lifetime W/L + rank | `constructedClass` | 6 | 1.4 KB |

**Total useful input is ~495 KB; the live log is 14 MB.** Everything else is
turn-by-turn game state that the tracker never reads.

### 1.2 "On play / on draw" is the die roll, not `turnInfo`

- There is **no** `onPlay` field in the log. An earlier grep matched 256 lines
  only because the substring sits inside `"decisi`onPlay`er"` — its count equals
  the `turnInfo` count exactly. It is a false positive; ignore it.
- The real signal is the die roll:

  ```
  GREMessageType_DieRollResultsResp
    dieRollResultsResp.playerDieRolls[ { systemSeatId, rollValue } ]
  ```

  **Higher `rollValue` wins and goes first (on the play); the other player is on
  the draw.** Confirmed against the current logs and against the `turnInfo`
  first-turn `activePlayer` proxy — the two always agree in these logs.

- The current parser infers on-play from the **first turn's `activePlayer`**
  (`turnInfo`). That is a valid proxy but it is indirect and expensive: the
  `turnInfo` marker is 57% of the file and only 15 of 256 `turnInfo` lines hold
  the first-turn value that is actually used.

### 1.3 Per-match line ordering (current log)

The useful messages arrive in a predictable order, each on its own line, and
the `gameRoomConfig` line repeats at match end:

```
match 27db4a84:
   line   281  config    players=['Sims 1', 'edgarallen']
   line   284  dieroll   seat1=16, seat2=12
   line   421  turn1     active=1
   line  4933  config    (repeat at match end)
```

Consequence: the die-roll winner is knowable from a **3-line-per-match** signal
that already contains the seat IDs, rather than from decoding multi-hundred-KB
`turnInfo` lines.

---

## 2. Cost of the current approach

`ParseLine` JSON-decodes **every** line matching a marker, including all 256
`turnInfo` lines. Only 15 of those carry a usable first turn.

Benchmark over the full current log (identical output, one scan per iteration):

| | Time / scan | Bytes alloc | Allocs |
|---|---|---|---|
| Current `ParseLine` | 59.2 ms | 23.6 MB | 13,680 |
| Marker-gated (only fields that matter) | 17.9 ms | 1.15 MB | 672 |

- `turnInfo` lines: 256 lines / **7.9 MB** decoded; only 15 lines / 463 KB used.
  **The other 241 lines / 7.49 MB are decoded and thrown away.**
- A single log line can be **1 MB+**; `scanner.Text()` copies each such line to a
  `string`, and the decode builds a struct graph on top of it — the source of the
  allocation spikes.

The cost is a **one-time startup scan** (the live tail is line-at-a-time), so it
is not user-visible latency. The real payoffs are **20× lower peak allocation /
GC pressure** and the correctness fix in §3.

---

## 3. Confirmed correctness hazard: the marker `switch` (ISSUES.md 1.2)

`ParseLine` is a mutually-exclusive `switch` on substrings. Real logs confirm the
fragility:

- A `finalMatchResult` line **also contains `gameRoomConfig`**; it only parses
  correctly because the `finalMatchResult` case is listed first. Reorder the
  cases — or introduce any future marker that co-occurs — and "won/lost" breaks
  **silently**, with no error.
- `turnInfo` and `gameRoomConfig` never co-occur in either log, so the
  `turnInfo` branch is reachable today — again only by luck.

Any performance rewrite must preserve or explicitly comment this ordering, or
make the branches independent.

---

## 4. Recommended changes

1. **Gate on cheap `bytes.Contains` before any `json.Unmarshal`.** The decode is
   the cost; the substring scan is ~free (0 allocs). Only decode a line that can
   yield one of the five fields. (~3.3× faster, ~20× less memory.)
2. **Use the die roll for on-play/on-draw.** Gate on `DieRollResultsResp`,
   decode, take `max(rollValue)` → on the play; compare its `systemSeatId` to the
   player's seat. Cheaper and more direct than first-turn `activePlayer`, and it
   cannot be confused by an app started mid-match.
3. **Scan as `[]byte`, not `string`.** `scanner.Text()` copies every line,
   including the 1 MB ones; `scanner.Bytes()` + `bytes.Contains` keeps the gate
   at 0 allocs.
4. **Decode from the `{` offset** (`line[i:]`) instead of slicing to a new
   `string`, to drop a second allocation per matched line.
5. **Fix the `switch` (ISSUES.md 1.2).** Make the branches independent or add an
   explicit comment that the order is load-bearing, so a future perf rewrite does
   not silently break won/lost.
6. **Decide on the `turnInfo` fallback.** Either keep first-turn `activePlayer`
   as a fallback when no die-roll line is present (robust to a mid-match start),
   or drop `turnInfo` entirely and rely solely on the die roll (leaner). Keeping
   it as a fallback is recommended.

Net result: a **marker-gated state machine over `[]byte`** that decodes only the
deck / result / seat / rank / die-roll lines, instead of decoding everything and
dispatching on a fragile substring `switch`.

---

## 5. Test fixtures available for locking this in (ISSUES.md 4.1)

The logs contain, per match, a clean config → die-roll → first-turn sequence with
known seat IDs and outcomes, e.g.:

| match | seat1 | seat2 | rolls (s1,s2) | roll winner |
|---|---|---|---|---|
| 27db4a84 | Sims 1 | edgarallen | 16, 12 | seat 1 |
| 0d62c31a | edgarallen | Temujin | 12, 16 | seat 2 |
| f890c972 | OGRE1365 | edgarallen | 6, 19 | seat 2 |
| 32101d6f | edgarallen | Les | 1, 14 | seat 2 |
| 7675b027 | Loreth | edgarallen | 1, 14 | seat 2 |

These make ideal table-driven test cases for the deck / play-draw / win / rank
fields.