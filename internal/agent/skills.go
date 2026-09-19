package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"charm.land/fantasy"
)

// maxListedDescription bounds one skill's description in the listing the skills
// tool returns. The listing is meant to be scanned, so a long description is
// cut; the skill itself, read by name, is always whole.
const maxListedDescription = 200

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

// LoadSkills reads every skill under dir into memory, sorted by name.
//
// A skill is a subdirectory containing a SKILL.md whose front matter supplies
// the name and description. A subdirectory without one is skipped rather than
// treated as an error - a skills folder routinely contains other things. dir
// itself must exist: it was named in the config, so a typo should stop the run
// at startup rather than quietly leave the model without its skills.
func LoadSkills(dir string) ([]Skill, error) {
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

	sort.Slice(skills, func(i, j int) bool { return skills[i].Name < skills[j].Name })

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

// skillsTool is the model's way to the skills: called with no name it lists them
// with their short descriptions, called with one it returns that skill's full
// instructions. Everything is already in memory, so a call reads no file.
func (s toolSet) skillsTool(skills []Skill) fantasy.AgentTool {
	return fantasy.NewAgentTool("skills",
		"Skills are ready-made instructions for particular kinds of work. Call with no arguments to list the available skills with a short description of each; check the list at the start of a task. Call with a skill's name to read its full instructions, then follow them.",
		func(_ context.Context, in skillsInput, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if strings.TrimSpace(in.Name) == "" {
				return fantasy.NewTextResponse(listSkills(skills)), nil
			}

			for _, skill := range skills {
				if skill.Name == in.Name {
					return fantasy.NewTextResponse(s.truncate(fmt.Sprintf("Skill directory: %s\n\n%s", skill.Dir, skill.Content))), nil
				}
			}

			return fantasy.NewTextErrorResponse(fmt.Sprintf("no skill named %q (available: %s)", in.Name, skillNames(skills))), nil
		})
}

// skillsInput is what the skills tool is called with; the struct is the schema.
type skillsInput struct {
	Name string `json:"name,omitempty" description:"The skill to read in full. Omit to list the available skills."`
}

// listSkills renders the listing: one line per skill, name then description.
func listSkills(skills []Skill) string {
	var b strings.Builder

	b.WriteString("Available skills - call again with a name to read one in full:\n")

	for _, skill := range skills {
		b.WriteString("\n- " + skill.Name)

		if skill.Description != "" {
			b.WriteString(": " + shorten(skill.Description, maxListedDescription))
		}
	}

	return b.String()
}

func skillNames(skills []Skill) string {
	names := make([]string, len(skills))

	for i, skill := range skills {
		names[i] = skill.Name
	}

	return strings.Join(names, ", ")
}

// shorten flattens text to one line and caps it at max characters, by rune so a
// multi-byte character is never cut in half.
func shorten(text string, max int) string {
	text = strings.Join(strings.Fields(text), " ")

	if utf8.RuneCountInString(text) <= max {
		return text
	}

	return string([]rune(text)[:max-1]) + "…"
}
