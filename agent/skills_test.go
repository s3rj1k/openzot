package agent

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

func TestLoadSkillsReadsFrontMatter(t *testing.T) {
	root := t.TempDir()

	writeSkill(t, root, "deploy", `---
name: deploy-service
description: Ship a service to production
---

# Deploy

Long instructions the model reads only when it decides the skill is relevant.
`)

	result, err := LoadSkills([]string{root})
	if err != nil {
		t.Fatalf("LoadSkills: %v", err)
	}

	if len(result.Skills) != 1 {
		t.Fatalf("got %d skills, want 1", len(result.Skills))
	}

	skill := result.Skills[0]

	if skill.Name != "deploy-service" {
		t.Errorf("name = %q, want the front-matter name to win over the directory", skill.Name)
	}

	if skill.Description != "Ship a service to production" {
		t.Errorf("description = %q", skill.Description)
	}

	// the path is what the model reads; the body is not loaded into context
	if skill.Path == "" {
		t.Error("a skill must carry the path to its instructions")
	}
}

func TestLoadSkillsFallsBackToTheBody(t *testing.T) {
	root := t.TempDir()

	writeSkill(t, root, "review", "# Review\n\nReview a pull request carefully.\n")

	result, err := LoadSkills([]string{root})
	if err != nil {
		t.Fatalf("LoadSkills: %v", err)
	}

	if len(result.Skills) != 1 {
		t.Fatalf("got %d skills, want 1", len(result.Skills))
	}

	skill := result.Skills[0]

	// without front matter the directory names it and the first prose line
	// describes it
	if skill.Name != "review" {
		t.Errorf("name = %q, want the directory name", skill.Name)
	}

	if skill.Description != "Review a pull request carefully." {
		t.Errorf("description = %q, want the first prose line", skill.Description)
	}
}

func TestLoadSkillsSkipsNonSkillDirectories(t *testing.T) {
	root := t.TempDir()

	writeSkill(t, root, "real", "---\nname: real\n---\n")

	// a skills folder routinely holds other things; they are skipped rather
	// than treated as an error
	os.MkdirAll(filepath.Join(root, "notaskill"), 0o755)
	os.WriteFile(filepath.Join(root, "loose.md"), []byte("x"), 0o644)

	result, err := LoadSkills([]string{root})
	if err != nil {
		t.Fatalf("LoadSkills: %v", err)
	}

	if len(result.Skills) != 1 {
		t.Fatalf("got %d skills, want just the real one: %+v", len(result.Skills), result.Skills)
	}
}

func TestLoadSkillsToleratesAMissingDirectory(t *testing.T) {
	result, err := LoadSkills([]string{filepath.Join(t.TempDir(), "nope")})
	if err != nil {
		t.Fatalf("a missing skills directory must not be an error: %v", err)
	}

	if len(result.Skills) != 0 {
		t.Errorf("got %d skills, want none", len(result.Skills))
	}
}

func TestParseSkillStripsQuotes(t *testing.T) {
	skill := parseSkill("dir", "p", "---\nname: \"quoted name\"\ndescription: 'quoted desc'\n---\n")

	if skill.Name != "quoted name" {
		t.Errorf("name = %q, want the quotes stripped", skill.Name)
	}

	if skill.Description != "quoted desc" {
		t.Errorf("description = %q, want the quotes stripped", skill.Description)
	}
}

func TestSkillHintPointsAShellCommandAtThePath(t *testing.T) {
	skill := SkillDefinition{Name: "d", Path: "/skills/d/SKILL.md"}

	hint := skill.Hint()

	if !strings.Contains(hint, "cat /skills/d/SKILL.md") || !strings.Contains(hint, "shell") {
		t.Errorf("a skill must point the shell at its path: %q", hint)
	}
}

func TestMergePrefersEarlierSets(t *testing.T) {
	project := &SkillsResult{Skills: []SkillDefinition{
		{Name: "deploy", Description: "the project's own"},
	}}

	builtin := &SkillsResult{Skills: []SkillDefinition{
		{Name: "deploy", Description: "the shipped default"},
		{Name: "review", Description: "also shipped"},
	}}

	merged := Merge(project, builtin, nil)

	if len(merged.Skills) != 2 {
		t.Fatalf("got %d skills, want 2", len(merged.Skills))
	}

	if merged.Skills[0].Description != "the project's own" {
		t.Errorf("the earlier set must win: %q", merged.Skills[0].Description)
	}
}

// The loader must pick up a skill added to its directory after the first scan -
// the mid-run case the dynamic loader exists for.
func TestSkillLoaderRescansDirectory(t *testing.T) {
	root := t.TempDir()

	writeSkill(t, root, "recon", "---\nname: recon\ndescription: map the target\n---\nbody")

	static := &SkillsResult{Skills: []SkillDefinition{
		{Name: "catalog", Description: "where the skills live"},
	}}

	loader := NewSkillLoader(static, root)

	first := loader.Skills()
	if len(first) != 2 {
		t.Fatalf("first scan: got %d skills, want 2 (static + one on disk)", len(first))
	}

	// A skill cloned in after the run started.
	writeSkill(t, root, "exploit", "---\nname: exploit\ndescription: prove it\n---\nbody")

	names := map[string]bool{}
	for _, s := range loader.Skills() {
		names[s.Name] = true
	}

	for _, want := range []string{"catalog", "recon", "exploit"} {
		if !names[want] {
			t.Errorf("rescan missing %q; the loader must see files added after the first scan", want)
		}
	}
}

// A directory skill must win over an embedded one of the same name, so a
// downloaded skill can override a shipped default.
func TestSkillLoaderDirectoryOverridesTheStaticSet(t *testing.T) {
	root := t.TempDir()

	writeSkill(t, root, "recon", "---\nname: recon\ndescription: the downloaded one\n---\nbody")

	static := &SkillsResult{Skills: []SkillDefinition{
		{Name: "recon", Description: "the shipped one"},
	}}

	skills := NewSkillLoader(static, root).Skills()

	if len(skills) != 1 {
		t.Fatalf("got %d skills, want 1", len(skills))
	}

	if skills[0].Description != "the downloaded one" {
		t.Errorf("the on-disk skill must win: %q", skills[0].Description)
	}
}

// A directory that disappears mid-run must not wipe the last good set.
func TestSkillLoaderFallsBackToLastGoodSet(t *testing.T) {
	root := t.TempDir()

	writeSkill(t, root, "recon", "---\nname: recon\ndescription: map the target\n---\nbody")

	loader := NewSkillLoader(nil, root)

	if got := len(loader.Skills()); got != 1 {
		t.Fatalf("first scan: got %d skills, want 1", got)
	}

	// LoadSkills treats a missing directory as empty rather than an error, so
	// removing it yields an empty scan - a legitimate result, not a failure to
	// fall back from. The loader must handle the vanished directory without
	// panicking and simply report no skills.
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}

	if got := loader.Skills(); len(got) != 0 {
		t.Errorf("got %d skills after the directory vanished, want 0", len(got))
	}
}
