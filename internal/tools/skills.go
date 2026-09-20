package tools

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"charm.land/fantasy"

	"github.com/openzot/openzot/internal/skills"
)

// maxListedDescription bounds one skill's description in the listing the skills
// tool returns. The listing is meant to be scanned, so a long description is
// cut; the skill itself, read by name, is always whole.
const maxListedDescription = 200

// skillsTool is the model's way to the skills: called with no name it lists them
// with their short descriptions, called with one it returns that skill's full
// instructions. Everything is already in memory, so a call reads no file.
func (s toolSet) skillsTool(offered []skills.Skill) fantasy.AgentTool {
	return fantasy.NewAgentTool("skills",
		"Skills are ready-made instructions for particular kinds of work. Call with no arguments to list the available skills with a short description of each; check the list at the start of a task. Call with a skill's name to read its full instructions, then follow them.",
		func(_ context.Context, in skillsInput, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if strings.TrimSpace(in.Name) == "" {
				return fantasy.NewTextResponse(listSkills(offered)), nil
			}

			for _, skill := range offered {
				if skill.Name == in.Name {
					return fantasy.NewTextResponse(s.truncate(fmt.Sprintf("Skill directory: %s\n\n%s", skill.Dir, skill.Content))), nil
				}
			}

			return fantasy.NewTextErrorResponse(fmt.Sprintf("no skill named %q (available: %s)", in.Name, skillNames(offered))), nil
		})
}

// skillsInput is what the skills tool is called with; the struct is the schema.
type skillsInput struct {
	Name string `json:"name,omitempty" description:"The skill to read in full. Omit to list the available skills."`
}

// listSkills renders the listing: one line per skill, name then description.
func listSkills(offered []skills.Skill) string {
	var b strings.Builder

	b.WriteString("Available skills - call again with a name to read one in full:\n")

	for _, skill := range offered {
		b.WriteString("\n- " + skill.Name)

		if skill.Description != "" {
			b.WriteString(": " + shorten(skill.Description, maxListedDescription))
		}
	}

	return b.String()
}

func skillNames(offered []skills.Skill) string {
	names := make([]string, len(offered))

	for i, skill := range offered {
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
