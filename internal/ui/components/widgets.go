package components

import (
	"fmt"
	"image/color"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/widget"
)

// StatCard builds a visually appealing metric card with custom colors.
type StatCard struct {
	Title   string
	Value   string
	Sub     string
	BgColor color.Color
}

// Render constructs a styled card component with a colored background.
func (s *StatCard) Render() fyne.CanvasObject {
	return container.NewStack(s.StatCardObjects()...)
}

// StatCardObjects returns the card's contents as a slice so callers can rebuild
// the card in place without replacing the whole widget tree.
func (s *StatCard) StatCardObjects() []fyne.CanvasObject {
	return StatCardObjects(s.Title, s.Value, s.Sub, s.BgColor)
}

// StatCardObjects builds the background and labels for a metric card.
func StatCardObjects(title, value, sub string, bgColor color.Color) []fyne.CanvasObject {
	bg := canvas.NewRectangle(bgColor)
	bg.CornerRadius = 8

	titleLabel := widget.NewLabelWithStyle(title, fyne.TextAlignCenter, fyne.TextStyle{Bold: true})
	valueLabel := widget.NewLabelWithStyle(value, fyne.TextAlignCenter, fyne.TextStyle{Bold: true})
	subLabel := widget.NewLabelWithStyle(sub, fyne.TextAlignCenter, fyne.TextStyle{Italic: true})

	content := container.NewPadded(container.NewVBox(titleLabel, valueLabel, subLabel))
	return []fyne.CanvasObject{bg, content}
}

// FormatWinRate calculates win percentage safely.
func FormatWinRate(wins, losses int) string {
	total := wins + losses
	if total == 0 {
		return "0.0%"
	}
	rate := (float64(wins) / float64(total)) * 100.0
	return fmt.Sprintf("%.1f%%", rate)
}
