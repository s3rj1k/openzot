// Package skills loads a folder of skills into memory: subdirectories each
// holding a SKILL.md of instructions the model reads when it decides the skill
// applies. Serving them to the model is the tools package's job.
package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Skill is one skill, loaded into memory at startup: a directory holding a
// SKILL.md of instructions the model reads when it decides the skill applies.
// Until it does, only the name and a short description reach the conversation.
type Skill struct {
	// Name is what the model asks for. The front matter's name, or the
	// directory's when there is none.
	Name string

	// Description is the short summary shown in the listing.
	Description string

	// Dir is the skill's own directory, which the model needs to find anything
	// the instructions refer to beside them.
	Dir string

	// Content is the whole SKILL.md.
	Content string
}

// Load reads every skill under dir into memory, sorted by name.
//
// A skill is a subdirectory containing a SKILL.md whose front matter supplies
// the name and description. A subdirectory without one is skipped rather than
// treated as an error - a skills folder routinely contains other things. Dir
// itself must exist: it was named in the config, so a typo should stop the run
// at startup rather than quietly leave the model without its skills.
func Load(dir string) ([]Skill, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	var (
		skills []Skill
		seen   = map[string]string{}
	)

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		skillDir := filepath.Join(dir, entry.Name())

		content, err := os.ReadFile(filepath.Join(skillDir, "SKILL.md"))
		if err != nil {
			continue
		}

		skill := parseSkill(entry.Name(), skillDir, string(content))

		if other, clash := seen[skill.Name]; clash {
			return nil, fmt.Errorf("skills %s and %s are both named %q", other, skillDir, skill.Name)
		}

		seen[skill.Name] = skillDir

		skills = append(skills, skill)
	}

	slices.SortFunc(skills, func(a, b Skill) int { return strings.Compare(a.Name, b.Name) })

	return skills, nil
}

// parseSkill reads the name and description out of a SKILL.md.
//
// Front matter is preferred; the first heading-free line is the fallback so a
// skill written without front matter still works.
func parseSkill(directoryName, dir, content string) Skill {
	skill := Skill{Name: directoryName, Dir: dir, Content: content}

	lines := strings.Split(content, "\n")

	if len(lines) > 0 && strings.TrimSpace(lines[0]) == "---" {
		for _, line := range lines[1:] {
			trimmed := strings.TrimSpace(line)

			if trimmed == "---" {
				break
			}

			key, value, found := strings.Cut(trimmed, ":")

			if !found {
				continue
			}

			value = strings.Trim(strings.TrimSpace(value), `"'`)

			switch strings.ToLower(strings.TrimSpace(key)) {
			case "name":
				if value != "" {
					skill.Name = value
				}
			case "description":
				skill.Description = value
			}
		}
	}

	if skill.Description == "" {
		for _, line := range lines {
			trimmed := strings.TrimSpace(line)

			if trimmed == "" || trimmed == "---" || strings.HasPrefix(trimmed, "#") {
				continue
			}

			skill.Description = trimmed

			break
		}
	}

	return skill
}
