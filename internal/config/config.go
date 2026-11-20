package config

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Defaults used when nothing can be discovered from the log or environment.
const (
	FallbackPlayerName = "edgarallen"

	// appDirName is the per-application subdirectory used under the XDG base
	// directories (data, state).
	appDirName = "arena-tail"
)

// DefaultDBPath returns the on-disk location of the SQLite database. It lives
// under the XDG data directory (not the current working directory) so that
// launching from a .desktop entry, whose CWD is typically "/" or $HOME, still
// finds the same database. The database holds lifetime history that is not
// rebuildable from the rotated logs, so it belongs in data rather than cache.
//
// Precedence: $ARENA_DB, then $XDG_DATA_HOME/arena-tail, then
// ~/.local/share/arena-tail.
func DefaultDBPath() (string, error) {
	if p := strings.TrimSpace(os.Getenv("ARENA_DB")); p != "" {
		return p, nil
	}
	dir, err := dataDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("creating data dir %s: %w", dir, err)
	}
	return filepath.Join(dir, "arena_tail.db"), nil
}

// dataDir resolves $XDG_DATA_HOME/arena-tail, falling back to
// ~/.local/share/arena-tail.
func dataDir() (string, error) {
	if base := strings.TrimSpace(os.Getenv("XDG_DATA_HOME")); base != "" {
		return filepath.Join(base, appDirName), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share", appDirName), nil
}

// AdoptLegacyDB moves a database left in the current working directory by an
// earlier version to the new XDG path. It only acts when the destination does
// not exist yet and a legacy arena_tail.db is present, so it can never
// overwrite real history. The legacy file (and any -wal/-shm sidecars) are
// copied rather than moved, leaving the original in place as a safety net.
//
// Returns the source path when a copy was made, and "" when there was nothing
// to do.
func AdoptLegacyDB(newPath string) (string, error) {
	if newPath == "" {
		return "", nil
	}
	if _, err := os.Stat(newPath); err == nil {
		return "", nil // destination already has history; leave it alone
	}

	const legacyName = "arena_tail.db"
	if _, err := os.Stat(legacyName); err != nil {
		return "", nil // no legacy DB in the working directory
	}

	if err := copyFile(legacyName, newPath); err != nil {
		return "", fmt.Errorf("adopting legacy database: %w", err)
	}
	// A WAL-mode database may have uncommitted pages in its sidecars; copying
	// all three keeps the adopted copy consistent.
	for _, suffix := range []string{"-wal", "-shm"} {
		src := legacyName + suffix
		if _, err := os.Stat(src); err != nil {
			continue
		}
		if err := copyFile(src, newPath+suffix); err != nil {
			return legacyName, fmt.Errorf("adopting legacy database sidecar %s: %w", suffix, err)
		}
	}
	return legacyName, nil
}

// copyFile copies src to dst preserving contents. The destination is created
// with 0600 because the database holds the player's screen name.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}

// discriminator matches the "#77660" style suffix MTGA shows in some surfaces.
// The real logs contain "edgarallen" with no suffix at all.
var discriminatorRE = regexp.MustCompile(`#\d+$`)

// NormalizePlayerName strips the numeric discriminator so a single player is
// stored under one key regardless of which surface produced the name.
func NormalizePlayerName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	return discriminatorRE.ReplaceAllString(name, "")
}

// logScreenNameRE pulls the screen name out of an MTGA AuthenticateResponse
// line. The field is written as "screenName":"edgarallen" with no discriminator.
var logScreenNameRE = regexp.MustCompile(`"screenName"\s*:\s*"([^"]+)"`)

// DiscoverScreenName reads the player's own screen name from an MTGA log file.
// The log is the authoritative source: it always records the bare name (for
// example "edgarallen") and never a "#1234" discriminator. Returns "" when the
// path is empty, unreadable, or no authentication line has been written yet.
func DiscoverScreenName(logPath string) string {
	if logPath == "" {
		return ""
	}
	f, err := os.Open(logPath)
	if err != nil {
		return ""
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	// MTGA lines (deck lists, game state) far exceed bufio's 64KB default; the
	// auth line is small, but the buffer must be large enough to reach it.
	scanner.Buffer(make([]byte, 0, 1024*1024), 16*1024*1024)
	for scanner.Scan() {
		m := logScreenNameRE.FindStringSubmatch(scanner.Text())
		if m == nil {
			continue
		}
		if name := NormalizePlayerName(m[1]); name != "" {
			return name
		}
	}
	// A scan error (I/O failure or an over-long line) must not be mistaken for
	// "no authentication line yet", which is the legitimate empty result.
	if err := scanner.Err(); err != nil {
		log.Printf("[CONFIG] reading screen name from %s: %v", logPath, err)
	}
	return ""
}

// logSubPath is where MTGA writes Player.log inside a Wine/Proton profile.
var logSubPath = filepath.Join(
	"AppData", "LocalLow", "Wizards Of The Coast", "MTGA",
)

// FindLogDir locates MTGA's log directory. It checks a native Linux install
// first, then walks the Steam Proton prefix for any user profile.
func FindLogDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}

	native := filepath.Join(home, ".local", "share", "Wizards Of The Coast", "MTGA")
	if dirExists(native) {
		return native
	}

	base := filepath.Join(
		home, ".local", "share", "Steam", "steamapps", "compatdata",
		"2141910", "pfx", "drive_c", "users",
	)
	entries, err := os.ReadDir(base)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		candidate := filepath.Join(base, e.Name(), logSubPath)
		if dirExists(candidate) {
			return candidate
		}
		// Some prefixes keep the log one level deeper under steamuser.
		candidate = filepath.Join(base, e.Name(), "steamuser", logSubPath)
		if dirExists(candidate) {
			return candidate
		}
	}
	return ""
}

// LogPaths returns the historical and live log files for a directory.
// Order matters: the previous log is older, so it is scanned first.
func LogPaths(dir string) (prev string, current string) {
	if dir == "" {
		return "", ""
	}
	return filepath.Join(dir, "Player-prev.log"), filepath.Join(dir, "Player.log")
}

func dirExists(path string) bool {
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
