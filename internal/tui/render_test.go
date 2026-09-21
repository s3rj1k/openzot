package tui_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/s3rj1k/agent/internal/testutils"
)

// The header shows the project you are in, not the machine you are on.
func TestTheDirStatShowsTheProjectEnd(t *testing.T) {
	m := sized(t, 400, 30)
	m.Workdir = "/workspaces/monorepo-agent/repos/agent/tool"

	bar := testutils.StripANSI(m.MetaBar())

	assert.Contains(t, bar, "tool", "the dir stat must keep the directory you are actually in")

	assert.NotContains(t, bar, "/workspaces/monorepo", "the shared leading path is what should be dropped")
}
