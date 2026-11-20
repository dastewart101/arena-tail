package views

import (
	"database/sql"
	"fmt"
	"image/color"
	"log"

	"arena-tail/internal/db"
	"arena-tail/internal/ui/components"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

// deckRowLimit caps how many decks load on first paint.
const deckRowLimit = 50

// deckRowHeight is the row-spacing knob.
const deckRowHeight float32 = 36

var (
	colorRank = color.NRGBA{R: 45, G: 25, B: 55, A: 255}
	colorLife = color.NRGBA{R: 55, G: 40, B: 20, A: 255}

	// Lighter surface behind the Deck Performance section.
	colorDeckPanel = color.RGBA{R: 0x2e, G: 0x2e, B: 0x38, A: 0xff}
	// A shade between panel and row, separating the header.
	colorDeckHeader = color.RGBA{R: 0x24, G: 0x24, B: 0x2e, A: 0xff}
)

var deckColumns = []struct {
	Title string
	Width float32
}{
	{"Deck", 440},
	{"W-L", 90},
	{"Win %", 80},
	{"On Play", 110},
	{"On Draw", 110},
}

type DashboardView struct {
	ScreenName string

	ConstructedClass string
	ConstructedLevel int
	Percentile       float64
	SeasonOrdinal    int
	BestMythicPct    float64
	HasBestMythic    bool

	LifetimeWins   int
	LifetimeLosses int

	DeckStats []db.DeckTotals

	db *sql.DB

	rankCard *fyne.Container
	lifeCard *fyne.Container

	deckTable     *widget.Table
	showAllButton *widget.Button
	showAllDecks  bool
	deckTruncated bool
}

func NewDashboardView(database *sql.DB, screenName string) *DashboardView {
	v := &DashboardView{
		ScreenName:       screenName,
		ConstructedClass: "Unranked",
		DeckStats:        make([]db.DeckTotals, 0),
		db:               database,
	}
	v.load()
	return v
}

func (v *DashboardView) Refresh() {
	v.load()

	if v.rankCard != nil {
		v.rankCard.Objects = v.rankCardObjects()
		v.rankCard.Refresh()
	}
	if v.lifeCard != nil {
		v.lifeCard.Objects = v.lifeCardObjects()
		v.lifeCard.Refresh()
	}
	if v.deckTable != nil {
		v.deckTable.Refresh()
	}
	if v.showAllButton != nil {
		if v.deckTruncated && !v.showAllDecks {
			v.showAllButton.Show()
		} else {
			v.showAllButton.Hide()
		}
	}
}

func (v *DashboardView) load() {
	if v.db == nil {
		return
	}

	if rank, err := db.Rank(v.db, v.ScreenName); err != nil {
		log.Printf("[UI] rank: %v", err)
	} else {
		v.ConstructedClass = rank.Class
		v.ConstructedLevel = rank.Level
		v.Percentile = rank.Percentile
		v.SeasonOrdinal = rank.SeasonOrdinal
		v.BestMythicPct = rank.BestMythicPct
		v.HasBestMythic = rank.HasBestMythic
		v.LifetimeWins = rank.LifetimeWins
		v.LifetimeLosses = rank.LifetimeLosses
	}

	limit := 0
	if !v.showAllDecks {
		limit = deckRowLimit + 1
	}
	stats, err := db.DecksPlayed(v.db, v.ScreenName, limit)
	if err != nil {
		log.Printf("[UI] deck stats: %v", err)
		return
	}
	v.deckTruncated = !v.showAllDecks && len(stats) > deckRowLimit
	if v.deckTruncated {
		stats = stats[:deckRowLimit]
	}
	v.DeckStats = stats
}

func (v *DashboardView) rankValue() string {
	if v.Percentile > 0 {
		return fmt.Sprintf("%s %.1f%%", v.ConstructedClass, v.Percentile)
	}
	return v.ConstructedClass
}

func (v *DashboardView) rankSub() string {
	sub := fmt.Sprintf("Level %d", v.ConstructedLevel)
	if v.SeasonOrdinal > 0 {
		sub = fmt.Sprintf("Season %d / Level %d", v.SeasonOrdinal, v.ConstructedLevel)
	}
	if v.HasBestMythic && v.BestMythicPct > v.Percentile {
		sub += fmt.Sprintf(" / Best %.1f%%", v.BestMythicPct)
	}
	return sub
}

func (v *DashboardView) rankCardObjects() []fyne.CanvasObject {
	return components.StatCardObjects("Rank Status", v.rankValue(), v.rankSub(), colorRank)
}

func (v *DashboardView) lifeCardObjects() []fyne.CanvasObject {
	total := v.LifetimeWins + v.LifetimeLosses
	return components.StatCardObjects(
		"Lifetime Record",
		fmt.Sprintf("%dW - %dL", v.LifetimeWins, v.LifetimeLosses),
		fmt.Sprintf("%d games / %s", total,
			components.FormatWinRate(v.LifetimeWins, v.LifetimeLosses)),
		colorLife)
}

// ellipsize shortens s to at most max runes, rune-based so multi-byte deck
// names are never split mid-character.
func ellipsize(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	if max <= 1 {
		return string(r[:max])
	}
	return string(r[:max-1]) + "…"
}

func (v *DashboardView) deckCell(col, row int) string {
	if col < 0 || col >= len(deckColumns) {
		return ""
	}
	if row == 0 {
		return deckColumns[col].Title
	}
	i := row - 1
	if i < 0 || i >= len(v.DeckStats) {
		return ""
	}
	d := v.DeckStats[i]
	switch col {
	case 0:
		return ellipsize(d.DeckName, 50)
	case 1:
		return fmt.Sprintf("%d - %d", d.Wins, d.Losses)
	case 2:
		return components.FormatWinRate(d.Wins, d.Losses)
	case 3:
		return fmt.Sprintf("%d - %d (%s)", d.PlayWins, d.PlayLosses,
			components.FormatWinRate(d.PlayWins, d.PlayLosses))
	case 4:
		return fmt.Sprintf("%d - %d (%s)", d.DrawWins, d.DrawLosses,
			components.FormatWinRate(d.DrawWins, d.DrawLosses))
	}
	return ""
}

// buildDeckTable builds the table once. Each cell is a background rectangle
// plus a label: the rectangle keeps rows on the window background while the
// section panel behind stays lighter, and its MinSize sets row height.
func (v *DashboardView) buildDeckTable() *widget.Table {
	t := widget.NewTable(
		func() (int, int) {
			return len(v.DeckStats) + 1, len(deckColumns)
		},
		func() fyne.CanvasObject {
			bg := canvas.NewRectangle(theme.Color(theme.ColorNameBackground))
			bg.SetMinSize(fyne.NewSize(1, deckRowHeight))
			l := widget.NewLabel("")
			return container.NewStack(bg, container.NewPadded(l))
		},
		func(id widget.TableCellID, o fyne.CanvasObject) {
			st, ok := o.(*fyne.Container)
			if !ok || len(st.Objects) < 2 {
				return
			}
			bg, ok := st.Objects[0].(*canvas.Rectangle)
			if !ok {
				return
			}
			padded, ok := st.Objects[1].(*fyne.Container)
			if !ok || len(padded.Objects) < 1 {
				return
			}
			l, ok := padded.Objects[0].(*widget.Label)
			if !ok {
				return
			}

			l.Text = v.deckCell(id.Col, id.Row)
			if id.Row == 0 {
				bg.FillColor = colorDeckHeader
				l.TextStyle = fyne.TextStyle{Bold: true}
			} else {
				bg.FillColor = theme.Color(theme.ColorNameBackground)
				if id.Col == 0 {
					l.TextStyle = fyne.TextStyle{Bold: true}
				} else {
					l.TextStyle = fyne.TextStyle{}
				}
			}
			bg.Refresh()
			l.Refresh()
		},
	)
	for col, spec := range deckColumns {
		t.SetColumnWidth(col, spec.Width)
	}
	return t
}

func (v *DashboardView) deckSection() fyne.CanvasObject {
	title := widget.NewLabelWithStyle("Deck Performance",
		fyne.TextAlignLeading, fyne.TextStyle{Bold: true})

	if len(v.DeckStats) == 0 {
		return container.NewVBox(title,
			widget.NewLabel("No deck history recorded yet."))
	}

	v.deckTable = v.buildDeckTable()
	v.showAllButton = widget.NewButton(
		fmt.Sprintf("Show all decks (showing %d)", deckRowLimit),
		func() {
			v.showAllDecks = true
			v.load()
			v.deckTable.Refresh()
			v.showAllButton.Hide()
		})
	if !v.deckTruncated {
		v.showAllButton.Hide()
	}

	// 830 is the sum of the column widths, so no horizontal scrollbar.
	body := container.NewBorder(
		container.NewVBox(title, v.showAllButton),
		nil, nil, nil,
		container.New(&minSizeLayout{min: fyne.NewSize(830, 260)}, v.deckTable),
	)

	panel := canvas.NewRectangle(colorDeckPanel)
	panel.CornerRadius = 8
	return container.NewStack(panel, container.NewPadded(body))
}

// minSizeLayout gives the table a floor size so it always has a scrollable
// viewport, while letting it grow with the window.
type minSizeLayout struct{ min fyne.Size }

func (m *minSizeLayout) Layout(objs []fyne.CanvasObject, size fyne.Size) {
	for _, o := range objs {
		o.Move(fyne.NewPos(0, 0))
		o.Resize(size)
	}
}

func (m *minSizeLayout) MinSize([]fyne.CanvasObject) fyne.Size {
	return m.min
}

func (v *DashboardView) BuildUI() fyne.CanvasObject {
	header := widget.NewLabelWithStyle(
		fmt.Sprintf("arena-tail | %s", v.ScreenName),
		fyne.TextAlignCenter, fyne.TextStyle{Bold: true})

	v.rankCard = container.NewStack(v.rankCardObjects()...)
	v.lifeCard = container.NewStack(v.lifeCardObjects()...)

	topGrid := container.NewGridWithColumns(2, v.rankCard, v.lifeCard)

	return container.NewBorder(
		container.NewVBox(
			container.NewPadded(header),
			widget.NewSeparator(),
			container.NewPadded(topGrid),
			widget.NewSeparator(),
		),
		nil, nil, nil,
		container.NewPadded(v.deckSection()),
	)
}
