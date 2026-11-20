package logger

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Log retention policy. These are deliberately conservative: debug logs are
// small and a single user generates few of them.
const (
	logFileName   = "arena-tail.log"
	maxLogBytes   = 5 << 20 // roll at 5 MB
	maxLogAge     = 14 * 24 * time.Hour
	maxLogFiles   = 3
	maxDirBytes   = 25 << 20
)

// SetupLogging redirects the standard logger to a file under the XDG state
// directory and returns a cleanup func to close it. Launched from a .desktop
// entry there is no terminal, so without this every diagnostic vanishes.
//
// When verbose is true output is mirrored to stderr as well, for interactive
// runs.
func SetupLogging(verbose bool) (func(), error) {
	dir, err := stateDir()
	if err != nil {
		return func() {}, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return func() {}, fmt.Errorf("creating log dir %s: %w", dir, err)
	}

	// Enforce retention before opening the new file so pruning never races
	// with the active handle.
	if err := pruneOldLogs(dir, maxLogAge, maxLogFiles, maxDirBytes); err != nil {
		// A prune failure must not stop logging from working.
		log.Printf("[LOGGER] pruning old logs: %v", err)
	}

	w, err := newRotatingWriter(filepath.Join(dir, logFileName), maxLogBytes)
	if err != nil {
		return func() {}, err
	}

	var out io.Writer = w
	if verbose {
		out = io.MultiWriter(w, os.Stderr)
	}
	log.SetOutput(out)
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.Printf("[LOGGER] logging to %s", w.path)

	return func() { _ = w.Close() }, nil
}

// stateDir resolves $XDG_STATE_HOME/arena-tail, falling back to
// ~/.local/state/arena-tail. State (not cache) is correct for logs that may be
// needed after a crash, since cache cleaners may delete ~/.cache at any time.
func stateDir() (string, error) {
	const dirName = "arena-tail"
	if base := strings.TrimSpace(os.Getenv("XDG_STATE_HOME")); base != "" {
		return filepath.Join(base, dirName), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", dirName), nil
}

// rotatingWriter is an io.Writer that rolls the underlying file to ".1" once it
// exceeds maxBytes. Only one previous file is kept, matching maxLogFiles.
type rotatingWriter struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	file     *os.File
	size     int64
}

func newRotatingWriter(path string, maxBytes int64) (*rotatingWriter, error) {
	w := &rotatingWriter{path: path, maxBytes: maxBytes}
	if err := w.open(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *rotatingWriter) open() error {
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("opening log file %s: %w", w.path, err)
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	w.file = f
	w.size = fi.Size()
	return nil
}

func (w *rotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.file == nil {
		if err := w.open(); err != nil {
			return 0, err
		}
	}
	// Roll *before* writing so a single entry is never split across files.
	if w.size > 0 && w.size+int64(len(p)) > w.maxBytes {
		if err := w.rotate(); err != nil {
			return 0, err
		}
	}

	n, err := w.file.Write(p)
	w.size += int64(n)
	return n, err
}

// rotate closes the active file, replaces the ".1" backup with it, and opens a
// fresh file. Errors from removing the old backup are ignored so rotation never
// blocks logging.
func (w *rotatingWriter) rotate() error {
	if w.file != nil {
		_ = w.file.Close()
		w.file = nil
	}
	backup := w.path + ".1"
	_ = os.Remove(backup)
	if err := os.Rename(w.path, backup); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("rotating log to %s: %w", backup, err)
	}
	w.size = 0
	return w.open()
}

func (w *rotatingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}

// pruneOldLogs enforces all three retention limits at once: age, file count and
// total bytes. It lives in one function so the rules are testable together and
// cannot diverge. Oldest files are deleted first. The active log file is
// preserved whenever it is the only one left.
func pruneOldLogs(dir string, maxAge time.Duration, maxCount int, maxBytes int64) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	type logFile struct {
		path    string
		size    int64
		modTime time.Time
	}
	var files []logFile
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, logFileName) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, logFile{
			path:    filepath.Join(dir, name),
			size:    info.Size(),
			modTime: info.ModTime(),
		})
	}
	if len(files) == 0 {
		return nil
	}

	// Newest first, so deletion from the tail removes the oldest.
	sort.Slice(files, func(i, j int) bool {
		return files[i].modTime.After(files[j].modTime)
	})

	cutoff := time.Now().Add(-maxAge)
	var (
		total   int64
		kept    int
		delErr  error
	)
	for _, f := range files {
		tooOld := f.modTime.Before(cutoff)
		tooMany := kept >= maxCount
		tooBig := total+f.size > maxBytes
		// Never delete the most recent file, even if every limit is exceeded:
		// it is the active log.
		if kept == 0 {
			total += f.size
			kept++
			continue
		}
		if tooOld || tooMany || tooBig {
			if err := os.Remove(f.path); err != nil && !os.IsNotExist(err) && delErr == nil {
				delErr = err
			}
			continue
		}
		total += f.size
		kept++
	}
	return delErr
}