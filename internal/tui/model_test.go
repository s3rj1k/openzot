package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
)

// The startup line shows before the first frame. Once sized the title badge
// takes over.
func TestStartupLineThenTitleBadge(t *testing.T) {
	m := newModel("hunt bugs", "model", "openai", "/tmp")

	got := m.View()
	assert.Contains(t, got, "starting zot…", "startup line must name zot, got %q", got)

	sized, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = asModel(t, sized)

	title := m.titleBar()
	assert.Contains(t, title, "zot", "title badge must name zot, got %q", title)
}
