package tui

import "github.com/charmbracelet/lipgloss"

// The palette is docs/ui-mockup.html's, so the terminal and the browser look
// like the same tool. Terminals vary, so these are the hex values lipgloss
// degrades to the nearest available colour.
var (
	colText  = lipgloss.Color("#c6d1dc")
	colDim   = lipgloss.Color("#7d8d9c")
	colGhost = lipgloss.Color("#4e5c69")
	colSteel = lipgloss.Color("#5b8cad")
	colError = lipgloss.Color("#e8705e")
	colWarn  = lipgloss.Color("#d9a341")
	colFatal = lipgloss.Color("#cf4a7e")
	colPanel = lipgloss.Color("#1e2833")
)

var (
	styleBrand = lipgloss.NewStyle().Foreground(colText).Bold(true)
	styleDim   = lipgloss.NewStyle().Foreground(colDim)
	styleGhost = lipgloss.NewStyle().Foreground(colGhost)
	styleSteel = lipgloss.NewStyle().Foreground(colSteel)
	styleWarn  = lipgloss.NewStyle().Foreground(colWarn)
	styleError = lipgloss.NewStyle().Foreground(colError)

	// styleHeader and styleFooter are the panel bars top and bottom.
	styleHeader = lipgloss.NewStyle().Foreground(colDim).Background(colPanel)
	styleFooter = lipgloss.NewStyle().Foreground(colGhost).Background(colPanel)

	// styleSelected marks the cursor row. Reverse video rather than a colour,
	// so it stays visible whatever the terminal theme is.
	styleSelected = lipgloss.NewStyle().Reverse(true)

	styleKey = lipgloss.NewStyle().Foreground(colGhost)
	styleVal = lipgloss.NewStyle().Foreground(colText)
	styleRaw = lipgloss.NewStyle().Foreground(colDim)
)

// levelStyle colours a severity the same way the CLI table and the web UI do.
func levelStyle(level string) lipgloss.Style {
	switch level {
	case "fatal":
		return lipgloss.NewStyle().Foreground(colFatal).Bold(true)
	case "error":
		return lipgloss.NewStyle().Foreground(colError)
	case "warn":
		return lipgloss.NewStyle().Foreground(colWarn)
	case "info":
		return lipgloss.NewStyle().Foreground(colSteel)
	case "debug", "trace":
		return styleGhost
	default:
		// An unrecognised level gets no colour: we do not know how serious it
		// is, and guessing would be worse than staying quiet.
		return styleDim
	}
}
