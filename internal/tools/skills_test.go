package tools

import (
	"strings"
	"testing"

	"github.com/openzot/openzot/internal/skills"
)

func skillsCall(t *testing.T, offered []skills.Skill, args map[string]any) (any, error) {
	t.Helper()

	return call(t, New(maxToolOutput, offered), "skills", args)
}

var testSkills = []skills.Skill{
	{Name: litDeploy, Description: "Ship a service", Dir: "/skills/deploy", Content: "# Deploy\n\nRun the pipeline.\n"},
	{Name: "review", Description: "Review a change", Dir: "/skills/review", Content: "# Review\n"},
}

func TestSkillsToolListsNamesWithDescriptions(t *testing.T) {
	out, err := skillsCall(t, testSkills, map[string]any{})
	if err != nil {
		t.Fatalf("skills: %v", err)
	}

	listing := asString(t, out)

	for _, want := range []string{"- deploy: Ship a service", "- review: Review a change"} {
		if !strings.Contains(listing, want) {
			t.Errorf("listing is missing %q:\n%s", want, listing)
		}
	}

	if strings.Contains(listing, "Run the pipeline") {
		t.Errorf("the listing must not carry the instructions themselves:\n%s", listing)
	}
}

func TestSkillsToolShortensALongDescription(t *testing.T) {
	long := strings.Repeat("word ", 100)

	out, _ := skillsCall(t, []skills.Skill{{Name: "verbose", Description: long}}, nil)

	line := asString(t, out)

	if strings.Count(line, "word") > maxListedDescription/5 || !strings.Contains(line, "…") {
		t.Errorf("a long description must be cut with an ellipsis:\n%s", line)
	}
}

func TestSkillsToolReadsOneInFull(t *testing.T) {
	out, err := skillsCall(t, testSkills, map[string]any{litName: litDeploy})
	if err != nil {
		t.Fatalf("skills: %v", err)
	}

	got := asString(t, out)

	if !strings.Contains(got, "Run the pipeline.") {
		t.Errorf("the full instructions are missing:\n%s", got)
	}

	if !strings.Contains(got, "Skill directory: /skills/deploy") {
		t.Errorf("the skill's directory is missing, so bundled files cannot be found:\n%s", got)
	}

	if strings.Contains(got, "Review") {
		t.Errorf("only the named skill may come back:\n%s", got)
	}
}

func TestSkillsToolNamesWhatExistsForAnUnknownSkill(t *testing.T) {
	_, err := skillsCall(t, testSkills, map[string]any{litName: "nope"})
	if err == nil {
		t.Fatal("an unknown skill must be an error the model can act on")
	}

	for _, want := range []string{`"nope"`, litDeploy, "review"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %s", err, want)
		}
	}
}

func TestSkillsToolBoundsWhatItReturns(t *testing.T) {
	big := []skills.Skill{{Name: "big", Content: strings.Repeat("x", 500)}}

	out, err := call(t, New(100, big), "skills", map[string]any{litName: "big"})
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(asString(t, out), "[truncated:") {
		t.Errorf("a skill larger than the tool ceiling must be visibly truncated: %q", out)
	}
}

// A run with no skills has no skills tool: nothing to list is not worth a tool
// in every request.
func TestTheSkillsToolExistsOnlyWhenThereAreSkills(t *testing.T) {
	if _, ok := findTool(New(maxToolOutput, nil), "skills"); ok {
		t.Error("no skills tool without skills")
	}

	if _, ok := findTool(New(maxToolOutput, testSkills), "skills"); !ok {
		t.Error("skills tool expected when skills are loaded")
	}
}
