package tui

import (
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
)

// A path is read from its end. The last segment names the project, the first
// are shared by every project on the machine. Cutting the head is the whole
// point - truncate does the opposite and was wrong here.
func TestShortPathKeepsTheInformativeEnd(t *testing.T) {
	tests := []struct {
		name string
		path string
		max  int
		want string
	}{
		{
			name: "a path that fits is left alone",
			path: litSrvAPI,
			max:  28,
			want: litSrvAPI,
		},
		{
			name: "leading segments are dropped, not trailing characters",
			path: "/workspaces/monorepo-zot/repos/zot/tool",
			max:  28,
			want: "…/repos/zot/tool",
		},
		{
			name: "segments are kept whole rather than half-named",
			path: "/home/vscode/development/projects/webservice",
			max:  20,
			want: "…/webservice",
		},
		{
			name: "a final segment that cannot fit is cut from the left",
			path: "/srv/an-extremely-long-directory-name-here",
			max:  12,
			want: "…y-name-here",
		},
		{
			name: "a trailing separator does not become an empty segment",
			path: "/workspaces/monorepo-zot/repos/zot/tool/",
			max:  28,
			want: "…/repos/zot/tool",
		},
		{
			name: "no room at all yields nothing rather than a stray ellipsis",
			path: litSrvAPI,
			max:  0,
			want: "",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := shortPath(test.path, test.max)

			assert.Equal(t, test.want, got)

			// whatever it returns must actually fit the budget it was given
			n := utf8.RuneCountInString(got)
			assert.LessOrEqual(t, n, test.max, "shortPath(%q, %d) is %d columns wide: %q", test.path, test.max, n, got)
		})
	}
}

// The header shows the project you are in, not the machine you are on.
func TestTheDirStatShowsTheProjectEnd(t *testing.T) {
	m := sized(t, 400, 30)
	m.workdir = "/workspaces/monorepo-zot/repos/zot/tool"

	bar := stripANSI(m.metaBar())

	assert.Contains(t, bar, "tool", "the dir stat must keep the directory you are actually in")

	assert.NotContains(t, bar, "/workspaces/monorepo", "the shared leading path is what should be dropped")
}
