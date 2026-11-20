package main

import (
	"bufio"
	"log"
	"os"
	"strings"

	"arena-tail/internal/config"
	"arena-tail/internal/db"
	"arena-tail/internal/logger"
	"arena-tail/internal/parser"
	"arena-tail/internal/ui/views"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
)

func main() {
	verbose := os.Getenv("ARENA_DEBUG") == "1"
	cleanup, err := logger.SetupLogging(verbose)
	if err != nil {
		log.Printf("[WARN] file logging unavailable, using stderr: %v", err)
	} else {
		defer cleanup()
	}

	myApp := app.New()
	myWindow := myApp.NewWindow("arena-tail | MTG Arena Tracker")

	logDir := config.FindLogDir()
	if logDir == "" {
		log.Printf("[WARN] could not locate the MTGA log directory")
	}
	prevLog, currentLog := config.LogPaths(logDir)

	playerName := resolvePlayerName(prevLog, currentLog)

	dbPath, err := config.DefaultDBPath()
	if err != nil {
		log.Fatalf("[FATAL] failed to resolve database path: %v", err)
	}
	if src, err := config.AdoptLegacyDB(dbPath); err != nil {
		log.Printf("[WARN] adopting legacy database: %v", err)
	} else if src != "" {
		log.Printf("[CONFIG] adopted legacy database from %s to %s", src, dbPath)
	}
	database, err := db.InitDB(dbPath)
	if err != nil {
		log.Fatalf("[FATAL] failed to initialize database at %s: %v", dbPath, err)
	}
	defer database.Close()

	dashboard := views.NewDashboardView(database, playerName)
	myWindow.SetContent(dashboard.BuildUI())
	// Taller default plus a resizable frame so the deck table has room to scroll.
	myWindow.Resize(fyne.NewSize(920, 620))
	myWindow.SetFixedSize(false)
	myWindow.CenterOnScreen()

	go func() {
		if prevLog != "" {
			if _, err := os.Stat(prevLog); err == nil {
				if err := scanFile(prevLog, parser.NewLogParser(database, playerName, "prev")); err != nil {
					log.Printf("[WARN] scanning %s: %v", prevLog, err)
				}
			}
		}
		fyne.Do(dashboard.Refresh)

		if currentLog == "" {
			log.Printf("[WARN] no log path resolved; live tail disabled")
			return
		}

		// Sample size before scanning so lines written during the scan are
		// re-read rather than skipped. match_id dedupe makes overlap harmless.
		var resumeOffset int64
		if fi, err := os.Stat(currentLog); err == nil {
			resumeOffset = fi.Size()
		}

		if err := scanFile(currentLog, parser.NewLogParser(database, playerName, "current")); err != nil {
			log.Printf("[WARN] scanning %s: %v", currentLog, err)
		}
		fyne.Do(dashboard.Refresh)

		lines := make(chan string, 500)
		go func() {
			defer close(lines)
			if err := logger.StartTailerAt(currentLog, resumeOffset, lines); err != nil {
				log.Printf("[WARN] tailer stopped: %v", err)
			}
		}()

		live := parser.NewLogParser(database, playerName, "current")
		for line := range lines {
			live.ParseLine(line)
			if isStatsEvent(line) {
				fyne.Do(dashboard.Refresh)
			}
		}
		log.Printf("[WARN] live tailing stopped; no further matches recorded until restart")
	}()

	myWindow.ShowAndRun()
}

// scanFile parses a whole log file. The buffer is enlarged because single MTGA
// lines (deck lists, game state) far exceed bufio's 64KB default.
func scanFile(path string, p *parser.LogParser) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 1024*1024), 16*1024*1024)
	for scanner.Scan() {
		p.ParseLine(scanner.Text())
	}
	return scanner.Err()
}

// isStatsEvent reports whether a live line can change the dashboard.
func isStatsEvent(line string) bool {
	switch {
	case strings.Contains(line, "finalMatchResult"):
		return true
	case strings.Contains(line, "constructedClass"):
		return true
	case strings.Contains(line, "EventSetDeckV3"):
		return true
	default:
		return false
	}
}

func resolvePlayerName(prevLog, currentLog string) string {
	if name := config.NormalizePlayerName(os.Getenv("ARENA_PLAYER")); name != "" {
		return name
	}
	if name := config.DiscoverScreenName(currentLog); name != "" {
		return name
	}
	if name := config.DiscoverScreenName(prevLog); name != "" {
		return name
	}
	return config.FallbackPlayerName
}
