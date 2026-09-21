package tui

import "github.com/charmbracelet/lipgloss"

// The brand color (accent) is zot's neutral identity. The rest are
// fixed semantic and neutral colors for state and log text.
var (
	colAccent = lipgloss.Color("#6B7280") // neutral slate (brand accent)
	colFg     = lipgloss.Color("#E6E6E6")
	colDim    = lipgloss.Color("#7A7A7A")
	colFaint  = lipgloss.Color("#4A4A4A")
	colGreen  = lipgloss.Color("#2DD4A7")
	colRed    = lipgloss.Color("#FF5A5A")
	colYellow = lipgloss.Color("#F2C94C")
	colCyan   = lipgloss.Color("#56C2FF")
	colBlue   = lipgloss.Color("#5B8CFF")
)

var (
	// Header.
	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#FFFFFF")).
			Background(colAccent).
			Padding(0, 1)

	taskStyle = lipgloss.NewStyle().Foreground(colFg)

	metaStyle = lipgloss.NewStyle().Foreground(colDim)
	metaKey   = lipgloss.NewStyle().Foreground(colFaint)

	// Meta-bar value colors. Fixed functional hues (not the brand accent), so the
	// header carries color and stays scannable even under a neutral theme. The
	// Model and provider are highlighted, the counters read by kind, and paths and
	// timing stay muted.
	metaModel    = lipgloss.NewStyle().Foreground(colCyan)
	metaProvider = lipgloss.NewStyle().Foreground(colBlue)
	metaCount    = lipgloss.NewStyle().Foreground(colFg)

	// Footer / hints.
	footerStyle = lipgloss.NewStyle().Foreground(colDim)
	keyHint     = lipgloss.NewStyle().Foreground(colCyan)

	// Status badges. Bold colored foreground (no background block) so the spinner
	// and label read as one solid color - embedding the spinner's own ANSI inside
	// a background style otherwise breaks the fill.
	statusRunningStyle = lipgloss.NewStyle().Bold(true).Foreground(colYellow)
	statusDoneStyle    = lipgloss.NewStyle().Bold(true).Foreground(colGreen)
	statusFailStyle    = lipgloss.NewStyle().Bold(true).Foreground(colRed)

	// Activity log.
	dividerStyle = lipgloss.NewStyle().Foreground(colFaint)
	thoughtStyle = lipgloss.NewStyle().Foreground(colDim).Italic(true)
	outputStyle  = lipgloss.NewStyle().Foreground(colFaint)
	okStyle      = lipgloss.NewStyle().Foreground(colGreen)
	errStyle     = lipgloss.NewStyle().Foreground(colRed)

	// Per-tool accents so the eye can scan the stream.
	toolExecStyle  = lipgloss.NewStyle().Foreground(colBlue)
	toolOtherStyle = lipgloss.NewStyle().Foreground(colAccent)
)
