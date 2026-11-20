package ui

import (
	"database/sql"
	"log"

	"arena-tail/internal/config"
	"arena-tail/internal/ui/views"
)

type App struct {
	DB         *sql.DB
	ScreenName string
	Dashboard  *views.DashboardView
}

// NewApp builds the UI shell for a player. The handle is normalized so a
// "#1234" discriminator can never reach the dashboard; MTGA logs record only
// the bare screen name (e.g. "edgarallen").
func NewApp(db *sql.DB, displayHandle string) *App {
	name := config.NormalizePlayerName(displayHandle)
	if name == "" {
		name = config.FallbackPlayerName
	}

	return &App{
		DB:         db,
		ScreenName: name,
		Dashboard:  views.NewDashboardView(db, name),
	}
}

func (a *App) RefreshData() {
	a.Dashboard = views.NewDashboardView(a.DB, a.ScreenName)
	log.Printf("[UI] Dashboard refreshed for %s", a.ScreenName)
}
