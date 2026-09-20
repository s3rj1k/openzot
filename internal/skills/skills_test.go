package skills

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

	skills, err := Load(root)
	if err != nil {
		t.Fatalf("Load: %v", err)
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

	writeSkill(t, root, "review", "# Review\n\nLook over a pull request carefully.\n")

	skills, err := Load(root)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if len(skills) != 1 || skills[0].Name != "review" {
		t.Fatalf("got %+v, want the directory name as the skill's name", skills)
	}

	if skills[0].Description != "Look over a pull request carefully." {
		t.Errorf("description = %q, want the first prose line", skills[0].Description)
	}
}

func TestLoadSkillsSkipsNonSkillsAndSortsTheRest(t *testing.T) {
	root := t.TempDir()

	writeSkill(t, root, "zebra", "---\nname: zebra\n---\n")
	writeSkill(t, root, "apple", "---\nname: apple\n---\n")

	// a skills folder routinely holds other things. They are skipped rather
	// than failing the load
	if err := os.MkdirAll(filepath.Join(root, "notaskill"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}

	skills, err := Load(root)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if len(skills) != 2 || skills[0].Name != "apple" || skills[1].Name != "zebra" {
		t.Fatalf("got %+v, want just the two real skills, sorted by name", skills)
	}
}

// The folder was named in the config, so one that cannot be read is an error
// rather than an empty set.
func TestLoadSkillsFailsOnAMissingDirectory(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Fatal("a missing skills directory must be an error")
	}
}

func TestLoadSkillsRefusesTwoSkillsWithOneName(t *testing.T) {
	root := t.TempDir()

	writeSkill(t, root, "a", "---\nname: same\n---\n")
	writeSkill(t, root, "b", "---\nname: same\n---\n")

	_, err := Load(root)
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
