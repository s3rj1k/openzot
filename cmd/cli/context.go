package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/openzot/openzot/internal/skills"
)

// The file agent looks for under each context directory.
const agentFile = "AGENTS.md"

// loadProjectContext reads the AGENTS.md found under the given directories, searched in order (typically the config directory
// then the working directory). Missing files are ignored and duplicate directories are searched once. The config's prompt
// decides whether and where to use them, as .Project.
func loadProjectContext(dirs ...string) string {
	seen := map[string]bool{}

	var found []string

	for _, dir := range dirs {
		if dir == "" || seen[dir] {
			continue
		}

		seen[dir] = true

		if data, err := os.ReadFile(filepath.Join(dir, agentFile)); err == nil { //nolint:gosec // G304: AGENTS.md in the config and project directories
			if text := strings.TrimSpace(string(data)); text != "" {
				found = append(found, text)
			}
		}
	}

	return strings.Join(found, "\n\n---\n\n")
}

// loadSkills reads the skills folder named by skills_dir. An unset skills_dir
// means no skills. A set one that cannot be read is an error, since the config
// asked for skills the run would otherwise silently lack.
func loadSkills(skillsDir string) ([]skills.Skill, error) {
	dir := strings.TrimSpace(skillsDir)
	if dir == "" {
		return nil, nil
	}

	if dir == "~" || strings.HasPrefix(dir, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("skills_dir %q: %w", skillsDir, err)
		}

		dir = filepath.Join(home, strings.TrimPrefix(dir, "~"))
	}

	loaded, err := skills.Load(dir)
	if err != nil {
		return nil, fmt.Errorf("skills_dir: %w", err)
	}

	return loaded, nil
}
