package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// SkillDefinition is a capability advertised to the model.
//
// Path points at the instructions rather than containing them. The model reads
// it only when it decides the skill is relevant, which keeps a large library
// cheap: until then, just the name and description occupy context.
type SkillDefinition struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Path        string `json:"path"`
}

// Hint is the instruction appended to a skill's description telling the model
// how to reach its instructions: with a shell command, at the skill's path.
func (s SkillDefinition) Hint() string {
	return fmt.Sprintf("Read it with a shell command, such as `cat %s`.", s.Path)
}

// SkillsResult is a loaded skill set.
type SkillsResult struct {
	Skills []SkillDefinition
}

// Merge combines skill sets, later entries losing to earlier ones on a name
// clash.
//
// The order matters and is deliberate: a project's own skill should win over a
// built-in one with the same name, which is what lets a repository override a
// shipped default rather than being stuck with it.
func Merge(sets ...*SkillsResult) *SkillsResult {
	merged := &SkillsResult{}

	seen := map[string]bool{}

	for _, set := range sets {
		if set == nil {
			continue
		}

		for _, skill := range set.Skills {
			if seen[skill.Name] {
				continue
			}

			seen[skill.Name] = true

			merged.Skills = append(merged.Skills, skill)
		}
	}

	return merged
}

// LoadSkills discovers skills in the given directories.
//
// A skill is a directory containing a SKILL.md whose front matter supplies the
// name and description. A directory without one is skipped rather than treated
// as an error - a skills folder routinely contains other things.
func LoadSkills(directories []string) (*SkillsResult, error) {
	result := &SkillsResult{}

	for _, directory := range directories {
		entries, err := os.ReadDir(directory)

		if err != nil {
			if os.IsNotExist(err) {
				continue
			}

			return nil, err
		}

		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}

			file := filepath.Join(directory, entry.Name(), "SKILL.md")

			content, err := os.ReadFile(file)
			if err != nil {
				continue
			}

			result.Skills = append(result.Skills, parseSkill(entry.Name(), file, string(content)))
		}
	}

	return result, nil
}

// SkillLoader serves a skill set that can change while a run is live.
//
// LoadSkills is a snapshot: it reads its directories once, so a skill added
// after startup - dropped in by the operator, or cloned by the agent itself
// mid-run - would never surface. The loader closes that gap by rescanning its
// directories on every call to Skills. The engine re-renders the system prompt
// each iteration, so handing it this method as the skills source makes a
// freshly added SKILL.md appear to the model on its very next turn, with no
// reload hook anyone has to remember to call.
//
// A rescan is a handful of small file reads - noise next to the model call
// that follows it - which is why there is no cache to invalidate and no
// watcher to wire up.
type SkillLoader struct {
	static      *SkillsResult
	directories []string

	mu   sync.Mutex
	last []SkillDefinition
}

// NewSkillLoader builds a loader over the given directories, layered on top of
// a static set - programmatic skills, typically - that is never rescanned. Either
// part may be empty or nil. On a name clash a directory skill wins, so a skill
// on disk can override a shipped default rather than being stuck with it.
func NewSkillLoader(static *SkillsResult, directories ...string) *SkillLoader {
	return &SkillLoader{static: static, directories: directories}
}

// Skills rescans the directories and returns the current set, merged with the
// static one. Its signature matches the engine's dynamic Skills option, so a
// caller passes the method value itself.
//
// A scan that fails outright falls back to the last good set rather than to
// nothing: losing the skill list mid-run because a directory turned unreadable
// would silently cost the model capabilities it was already using.
func (l *SkillLoader) Skills() []SkillDefinition {
	l.mu.Lock()
	defer l.mu.Unlock()

	scanned, err := LoadSkills(l.directories)
	if err != nil {
		if l.last != nil {
			return l.last
		}

		scanned = &SkillsResult{}
	}

	l.last = Merge(scanned, l.static).Skills

	return l.last
}

// parseSkill reads the name and description out of a SKILL.md.
//
// Front matter is preferred; the first heading and paragraph are the fallback so
// a skill written without front matter still works.
func parseSkill(directory, location, content string) SkillDefinition {
	skill := SkillDefinition{Name: directory, Path: location}

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
