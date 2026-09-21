package skills_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/s3rj1k/agent/internal/skills"
)

func writeSkill(t *testing.T, root, name, content string) {
	t.Helper()

	dir := filepath.Join(root, name)

	require.NoError(t, os.MkdirAll(dir, 0o755))

	require.NoError(t, os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644))
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

	loaded, err := skills.Load(root)
	require.NoError(t, err)

	require.Len(t, loaded, 1)

	skill := loaded[0]

	assert.Equal(t, "deploy-service", skill.Name, "want the front-matter name to win over the directory")

	assert.Equal(t, "Ship a service to production", skill.Description)

	assert.Equal(t, body, skill.Content, "want the whole SKILL.md held in memory")

	assert.Equal(t, filepath.Join(root, "deploy"), skill.Dir, "want the skill's own directory")
}

func TestLoadSkillsFallsBackToTheBody(t *testing.T) {
	root := t.TempDir()

	writeSkill(t, root, "review", "# Review\n\nLook over a pull request carefully.\n")

	loaded, err := skills.Load(root)
	require.NoError(t, err)

	require.Len(t, loaded, 1)
	require.Equal(t, "review", loaded[0].Name)

	assert.Equal(t, "Look over a pull request carefully.", loaded[0].Description, "want the first prose line")
}

func TestLoadSkillsSkipsNonSkillsAndSortsTheRest(t *testing.T) {
	root := t.TempDir()

	writeSkill(t, root, "zebra", "---\nname: zebra\n---\n")
	writeSkill(t, root, "apple", "---\nname: apple\n---\n")

	// a skills folder routinely holds other things. They are skipped rather
	// than failing the load
	require.NoError(t, os.MkdirAll(filepath.Join(root, "notaskill"), 0o755))

	require.NoError(t, os.WriteFile(filepath.Join(root, "README.md"), []byte("hi"), 0o644))

	loaded, err := skills.Load(root)
	require.NoError(t, err)

	require.Len(t, loaded, 2, "want just the two real skills, sorted by name")
	require.Equal(t, "apple", loaded[0].Name, "want just the two real skills, sorted by name")
	require.Equal(t, "zebra", loaded[1].Name, "want just the two real skills, sorted by name")
}

// The folder was named in the config, so one that cannot be read is an error
// rather than an empty set.
func TestLoadSkillsFailsOnAMissingDirectory(t *testing.T) {
	_, err := skills.Load(filepath.Join(t.TempDir(), "nope"))
	require.Error(t, err, "a missing skills directory must be an error")
}

func TestLoadSkillsRefusesTwoSkillsWithOneName(t *testing.T) {
	root := t.TempDir()

	writeSkill(t, root, "a", "---\nname: same\n---\n")
	writeSkill(t, root, "b", "---\nname: same\n---\n")

	_, err := skills.Load(root)
	require.Error(t, err, "want it to name the clashing skill")
	require.Contains(t, err.Error(), `"same"`, "want it to name the clashing skill")
}

func TestParseSkillStripsQuotes(t *testing.T) {
	skill := skills.ParseSkill("dir", "/d", "---\nname: \"quoted name\"\ndescription: 'quoted desc'\n---\n")

	assert.Equal(t, "quoted name", skill.Name)

	assert.Equal(t, "quoted desc", skill.Description)
}
