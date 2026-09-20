package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSkill(t *testing.T, root, name, content string) {
	t.Helper()

	dir := filepath.Join(root, name)

	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadSkillsReadsFrontMatterAndKeepsTheContent(t *testing.T) {
	root := t.TempDir()

	body := `---
name: deploy-service
description: Ship a service to production
---
# Deploy

Long instructions the model reads only when it decides the skill is relevant.
`

	writeSkill(t, root, "deploy", body)

	skills, err := LoadSkills(root)
	if err != nil {
		t.Fatalf("LoadSkills: %v", err)
	}

	if len(skills) != 1 {
		t.Fatalf("got %d skills, want 1", len(skills))
	}

	skill := skills[0]

	if skill.Name != "deploy-service" {
		t.Errorf("name = %q, want the front-matter name to win over the directory", skill.Name)
	}

	if skill.Description != "Ship a service to production" {
		t.Errorf("description = %q", skill.Description)
	}

	if skill.Content != body {
		t.Errorf("content = %q, want the whole SKILL.md held in memory", skill.Content)
	}

	if skill.Dir != filepath.Join(root, "deploy") {
		t.Errorf("dir = %q, want the skill's own directory", skill.Dir)
	}
}

func TestLoadSkillsFallsBackToTheBody(t *testing.T) {
	root := t.TempDir()

	writeSkill(t, root, "review", "# Review\n\nReview a pull request carefully.\n")

	skills, err := LoadSkills(root)
	if err != nil {
		t.Fatalf("LoadSkills: %v", err)
	}

	if len(skills) != 1 || skills[0].Name != "review" {
		t.Fatalf("got %+v, want the directory name as the skill's name", skills)
	}

	if skills[0].Description != "Review a pull request carefully." {
		t.Errorf("description = %q, want the first prose line", skills[0].Description)
	}
}

func TestLoadSkillsSkipsNonSkillsAndSortsTheRest(t *testing.T) {
	root := t.TempDir()

	writeSkill(t, root, "zebra", "---\nname: zebra\n---\n")
	writeSkill(t, root, "apple", "---\nname: apple\n---\n")

	// a skills folder routinely holds other things; they are skipped rather
	// than failing the load
	if err := os.MkdirAll(filepath.Join(root, "notaskill"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}

	skills, err := LoadSkills(root)
	if err != nil {
		t.Fatalf("LoadSkills: %v", err)
	}

	if len(skills) != 2 || skills[0].Name != "apple" || skills[1].Name != "zebra" {
		t.Fatalf("got %+v, want just the two real skills, sorted by name", skills)
	}
}

// The folder was named in the config, so one that cannot be read is an error
// rather than an empty set.
func TestLoadSkillsFailsOnAMissingDirectory(t *testing.T) {
	if _, err := LoadSkills(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Fatal("a missing skills directory must be an error")
	}
}

func TestLoadSkillsRefusesTwoSkillsWithOneName(t *testing.T) {
	root := t.TempDir()

	writeSkill(t, root, "a", "---\nname: same\n---\n")
	writeSkill(t, root, "b", "---\nname: same\n---\n")

	_, err := LoadSkills(root)
	if err == nil || !strings.Contains(err.Error(), `"same"`) {
		t.Fatalf("err = %v, want it to name the clashing skill", err)
	}
}

func TestParseSkillStripsQuotes(t *testing.T) {
	skill := parseSkill("dir", "/d", "---\nname: \"quoted name\"\ndescription: 'quoted desc'\n---\n")

	if skill.Name != "quoted name" {
		t.Errorf("name = %q, want the quotes stripped", skill.Name)
	}

	if skill.Description != "quoted desc" {
		t.Errorf("description = %q, want the quotes stripped", skill.Description)
	}
}

func skillsCall(t *testing.T, skills []Skill, args map[string]any) (any, error) {
	t.Helper()

	return call(t, New(maxToolOutput, skills), "skills", args)
}

var testSkills = []Skill{
	{Name: "deploy", Description: "Ship a service", Dir: "/skills/deploy", Content: "# Deploy\n\nRun the pipeline.\n"},
	{Name: "review", Description: "Review a change", Dir: "/skills/review", Content: "# Review\n"},
}

func TestSkillsToolListsNamesWithDescriptions(t *testing.T) {
	out, err := skillsCall(t, testSkills, map[string]any{})
	if err != nil {
		t.Fatalf("skills: %v", err)
	}

	listing := out.(string)

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

	out, _ := skillsCall(t, []Skill{{Name: "verbose", Description: long}}, nil)

	line := out.(string)

	if strings.Count(line, "word") > maxListedDescription/5 || !strings.Contains(line, "…") {
		t.Errorf("a long description must be cut with an ellipsis:\n%s", line)
	}
}

func TestSkillsToolReadsOneInFull(t *testing.T) {
	out, err := skillsCall(t, testSkills, map[string]any{"name": "deploy"})
	if err != nil {
		t.Fatalf("skills: %v", err)
	}

	got := out.(string)

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
	_, err := skillsCall(t, testSkills, map[string]any{"name": "nope"})
	if err == nil {
		t.Fatal("an unknown skill must be an error the model can act on")
	}

	for _, want := range []string{`"nope"`, "deploy", "review"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %s", err, want)
		}
	}
}

func TestSkillsToolBoundsWhatItReturns(t *testing.T) {
	big := []Skill{{Name: "big", Content: strings.Repeat("x", 500)}}

	out, err := call(t, New(100, big), "skills", map[string]any{"name": "big"})
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(out.(string), "[truncated:") {
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
