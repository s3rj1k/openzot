package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// The app name identifies the embedding application throughout the view, so a
// host (rook, pion) reads as itself rather than "zot". It appears in the startup
// line before the first frame and in the title badge once sized.
func TestAppNameRendersInStartupAndTitle(t *testing.T) {
	m := newModel("rook", "hunt bugs", "model", "openai", "/tmp")

	if got := m.View(); !strings.Contains(got, "starting rook…") {
		t.Errorf("startup line must carry the app name, got %q", got)
	}

	sized, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = sized.(model)

	if title := m.titleBar(); !strings.Contains(title, "rook") {
		t.Errorf("title badge must carry the app name, got %q", title)
	}
}
