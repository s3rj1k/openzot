package tui

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/s3rj1k/agent/internal/render"
)

func (m *Model) Badge() string {
	switch m.Status {
	case StatusDone:
		return render.StatusDoneStyle.Render("✓ done")
	case StatusFailed:
		return render.StatusFailStyle.Render("✗ failed")
	default:
		// Keep the spinner and label as separate same-color pieces. Nesting the
		// spinner's own ANSI inside another style breaks the run of color.
		return m.spinner.View() + render.StatusRunningStyle.Render("working")
	}
}

func (m *Model) TitleBar() string {
	left := render.TitleStyle.Render("✦ agent") + " " + m.Badge()

	room := m.Width - lipgloss.Width(left) - 2
	if room < 8 {
		return left
	}
	// A title is what the header wants. The task is the whole order rendered
	// for the model, so a one-line header of it is a paragraph cut mid-word.
	label := m.Title
	if label == "" {
		label = m.Task
	}

	return left + " " + render.TaskStyle.Render(render.Truncate(label, room))
}

// cell pads v on the right to at least width columns, so a value that changes length keeps the segments after it
// where they were. The value stays flush against its label, and one wider than the cell is returned whole.
func cell(v string, width int) string {
	if pad := width - lipgloss.Width(v); pad > 0 {
		return v + strings.Repeat(" ", pad)
	}

	return v
}

// MetaBar is the header, provider, model, iteration, elapsed time, tokens and directory, in that order. The bar drops
// what does not fit, so what comes first survives a narrow terminal, and dir is last because it never changes.
func (m *Model) MetaBar() string {
	seg := func(k, v string, value lipgloss.Style) string {
		return render.MetaKey.Render(k+" ") + value.Render(v)
	}

	// iterations renders "n" or "n/max" when a limit is set, so progress against a
	// configured budget is visible.
	iterations := strconv.Itoa(m.Iteration)
	iterationsWidth := 4

	if m.MaxIterations > 0 {
		iterations = fmt.Sprintf("%d/%d", m.Iteration, m.MaxIterations)
		iterationsWidth = lipgloss.Width(fmt.Sprintf("%d/%d", m.MaxIterations, m.MaxIterations))
	}

	elapsed := render.FmtDuration(m.Elapsed)
	if m.MaxDuration > 0 {
		elapsed += "/" + render.FmtDuration(m.MaxDuration)
	}

	// Live values sit in fixed-width cells (see cell) so a number gaining a digit does not shove every later
	// segment sideways. A value that outgrows its cell renders whole, and the bar shifts once rather than clipping.
	segments := []string{
		seg("provider", m.provider, render.MetaProvider),
		seg("model", m.model, render.MetaModel),
		seg("iter", cell(iterations, iterationsWidth), render.MetaCount),
		seg("elapsed", elapsed, render.MetaStyle),
		seg("tokens", fmt.Sprintf("↑%s ↓%s", cell(render.FmtTokens(m.InputTokens), 6), cell(render.FmtTokens(m.OutputTokens), 6)), render.MetaModel),
		seg("dir", render.ShortPath(m.Workdir, 28), render.MetaStyle),
	}

	// A segment is shown whole or not at all, since clipping left half-rendered segments ("elap", "tok") that
	// read as a broken UI. The bar takes segments in order while they fit and stops at the first that does not.
	separator := render.MetaStyle.Render("  ·  ")
	separatorWidth := lipgloss.Width(separator)

	parts := make([]string, 0, len(segments))
	used := 0

	for _, segment := range segments {
		needed := lipgloss.Width(segment)
		if len(parts) > 0 {
			needed += separatorWidth
		}

		// Width is zero until the first WindowSizeMsg arrives. There is no
		// terminal to fit yet, so nothing is dropped for not fitting it.
		if m.Width > 0 && used+needed > m.Width {
			break
		}

		parts = append(parts, segment)
		used += needed
	}

	line := strings.Join(parts, separator)

	return lipgloss.NewStyle().MaxWidth(m.Width).Render(line)
}

func (m *Model) Footer() string {
	hints := render.FooterStyle.Render(
		render.KeyHint.Render("↑/↓") + " scroll  " +
			render.KeyHint.Render("g/G") + " top/bottom  " +
			render.KeyHint.Render("q") + " quit",
	)
	if m.Status == StatusRunning {
		return hints
	}

	tail := render.FooterStyle.Render("  ·  press " + render.KeyHint.Render("q") + " to exit")

	return hints + tail
}
