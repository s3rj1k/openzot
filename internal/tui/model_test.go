package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// The startup line shows before the first frame; once sized the title badge
// takes over.
func TestStartupLineThenTitleBadge(t *testing.T) {
	m := newModel("hunt bugs", "model", "openai", "/tmp")

	if got := m.View(); !strings.Contains(got, "starting zot…") {
		t.Errorf("startup line must name zot, got %q", got)
	}

	sized, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = asModel(t, sized)

	if title := m.titleBar(); !strings.Contains(title, "zot") {
		t.Errorf("title badge must name zot, got %q", title)
	}
}
