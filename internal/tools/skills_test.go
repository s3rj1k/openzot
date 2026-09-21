package tools_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openzot/openzot/internal/skills"
	"github.com/openzot/openzot/internal/tools"
)

func skillsCall(t *testing.T, offered []skills.Skill, args map[string]any) (any, error) {
	t.Helper()

	return call(t, tools.New(maxToolOutput, offered), "skills", args)
}

var testSkills = []skills.Skill{
	{Name: litDeploy, Description: "Ship a service", Dir: "/skills/deploy", Content: "# Deploy\n\nRun the pipeline.\n"},
	{Name: "review", Description: "Review a change", Dir: "/skills/review", Content: "# Review\n"},
}

func TestSkillsToolListsNamesWithDescriptions(t *testing.T) {
	out, err := skillsCall(t, testSkills, map[string]any{})
	require.NoError(t, err)

	listing := asString(t, out)

	for _, want := range []string{"- deploy: Ship a service", "- review: Review a change"} {
		assert.Contains(t, listing, want)
	}

	assert.NotContains(t, listing, "Run the pipeline", "the listing must not carry the instructions themselves")
}

func TestSkillsToolShortensALongDescription(t *testing.T) {
	long := strings.Repeat("word ", 100)

	out, _ := skillsCall(t, []skills.Skill{{Name: "verbose", Description: long}}, nil)

	line := asString(t, out)

	assert.LessOrEqual(t, strings.Count(line, "word"), tools.MaxListedDescription/5, "a long description must be cut with an ellipsis")
	assert.Contains(t, line, "…", "a long description must be cut with an ellipsis")
}

func TestSkillsToolReadsOneInFull(t *testing.T) {
	out, err := skillsCall(t, testSkills, map[string]any{litName: litDeploy})
	require.NoError(t, err)

	got := asString(t, out)

	assert.Contains(t, got, "Run the pipeline.", "the full instructions are missing")

	assert.Contains(t, got, "Skill directory: /skills/deploy", "the skill's directory is missing, so bundled files cannot be found")

	assert.NotContains(t, got, "Review", "only the named skill may come back")
}

func TestSkillsToolNamesWhatExistsForAnUnknownSkill(t *testing.T) {
	_, err := skillsCall(t, testSkills, map[string]any{litName: "nope"})
	require.Error(t, err, "an unknown skill must be an error the model can act on")

	for _, want := range []string{`"nope"`, litDeploy, "review"} {
		assert.Contains(t, err.Error(), want)
	}
}

func TestSkillsToolBoundsWhatItReturns(t *testing.T) {
	big := []skills.Skill{{Name: "big", Content: strings.Repeat("x", 500)}}

	out, err := call(t, tools.New(100, big), "skills", map[string]any{litName: "big"})
	require.NoError(t, err)

	assert.Contains(t, asString(t, out), "[truncated:", "a skill larger than the tool ceiling must be visibly truncated")
}

// A run with no skills has no skills tool. Nothing to list is not worth a tool
// in every request.
func TestTheSkillsToolExistsOnlyWhenThereAreSkills(t *testing.T) {
	_, ok := findTool(tools.New(maxToolOutput, nil), "skills")
	assert.False(t, ok, "no skills tool without skills")

	_, ok = findTool(tools.New(maxToolOutput, testSkills), "skills")
	assert.True(t, ok, "skills tool expected when skills are loaded")
}
