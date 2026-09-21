package render

import "github.com/charmbracelet/lipgloss"

// The brand color (accent) is agent's neutral identity. The rest are
// fixed semantic and neutral colors for state and log text.
var (
	colAccent = lipgloss.Color("#6B7280") // neutral slate (brand accent)
	colFg     = lipgloss.Color("#E6E6E6")
	colDim    = lipgloss.Color("#7A7A7A")
	colFaint  = lipgloss.Color("#4A4A4A")
	colGreen  = lipgloss.Color("#2DD4A7")
	colRed    = lipgloss.Color("#FF5A5A")
	ColYellow = lipgloss.Color("#F2C94C")
	colCyan   = lipgloss.Color("#56C2FF")
	colBlue   = lipgloss.Color("#5B8CFF")
)

var (
	// Header.
	TitleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#FFFFFF")).
			Background(colAccent).
			Padding(0, 1)

	TaskStyle = lipgloss.NewStyle().Foreground(colFg)

	MetaStyle = lipgloss.NewStyle().Foreground(colDim)
	MetaKey   = lipgloss.NewStyle().Foreground(colFaint)

	// Meta-bar value colors. Fixed functional hues (not the brand accent), so the
	// header carries color and stays scannable even under a neutral theme. The
	// Model and provider are highlighted, the counters read by kind, and paths and
	// timing stay muted.
	MetaModel    = lipgloss.NewStyle().Foreground(colCyan)
	MetaProvider = lipgloss.NewStyle().Foreground(colBlue)
	MetaCount    = lipgloss.NewStyle().Foreground(colFg)

	// Footer / hints.
	FooterStyle = lipgloss.NewStyle().Foreground(colDim)
	KeyHint     = lipgloss.NewStyle().Foreground(colCyan)

	// Status badges. Bold colored foreground (no background block) so the spinner
	// and label read as one solid color - embedding the spinner's own ANSI inside
	// a background style otherwise breaks the fill.
	StatusRunningStyle = lipgloss.NewStyle().Bold(true).Foreground(ColYellow)
	StatusDoneStyle    = lipgloss.NewStyle().Bold(true).Foreground(colGreen)
	StatusFailStyle    = lipgloss.NewStyle().Bold(true).Foreground(colRed)

	// Activity log.
	DividerStyle = lipgloss.NewStyle().Foreground(colFaint)
	ThoughtStyle = lipgloss.NewStyle().Foreground(colDim).Italic(true)
	OutputStyle  = lipgloss.NewStyle().Foreground(colFaint)
	OkStyle      = lipgloss.NewStyle().Foreground(colGreen)
	ErrStyle     = lipgloss.NewStyle().Foreground(colRed)

	// Per-tool accents so the eye can scan the stream.
	toolExecStyle  = lipgloss.NewStyle().Foreground(colBlue)
	toolOtherStyle = lipgloss.NewStyle().Foreground(colAccent)
)
