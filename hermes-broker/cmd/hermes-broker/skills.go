package main

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
)

type CatalogSkill struct {
	Name           string `json:"name"`
	Description    string `json:"description"`
	Required       bool   `json:"required"`
	DefaultEnabled bool   `json:"default_enabled"`
}

type Catalog struct {
	Version string         `json:"version"`
	Skills  []CatalogSkill `json:"skills"`
}

// LoadCatalog reads catalog.json from the broker's copy of the catalog image.
func LoadCatalog(path string) (*Catalog, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Catalog
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Catalog) skill(name string) *CatalogSkill {
	for i := range c.Skills {
		if c.Skills[i].Name == name {
			return &c.Skills[i]
		}
	}
	return nil
}

// Enabled applies the same rule as hermes-sync: required, or default-on and
// not disabled, or default-off and explicitly enabled.
func (c *Catalog) Enabled(sel Selection) map[string]bool {
	on := map[string]bool{}
	for _, s := range c.Skills {
		switch {
		case s.Required:
			on[s.Name] = true
		case s.DefaultEnabled:
			on[s.Name] = !slices.Contains(sel.Disabled, s.Name)
		default:
			on[s.Name] = slices.Contains(sel.Enabled, s.Name)
		}
	}
	return on
}

// Apply returns the selection after `/skills on|off` for names, and the
// names it refused or did not know.
func (c *Catalog) Apply(sel Selection, on bool, names []string) (Selection, []string) {
	var refused []string
	out := Selection{Disabled: slices.Clone(sel.Disabled), Enabled: slices.Clone(sel.Enabled)}
	for _, n := range names {
		s := c.skill(n)
		switch {
		case s == nil:
			refused = append(refused, n+" (not in catalog)")
			continue
		case s.Required && !on:
			refused = append(refused, n+" (required)")
			continue
		}
		out.Disabled = slices.DeleteFunc(out.Disabled, func(x string) bool { return x == n })
		out.Enabled = slices.DeleteFunc(out.Enabled, func(x string) bool { return x == n })
		if on && !s.DefaultEnabled {
			out.Enabled = append(out.Enabled, n)
		}
		if !on && s.DefaultEnabled {
			out.Disabled = append(out.Disabled, n)
		}
	}
	slices.Sort(out.Disabled)
	slices.Sort(out.Enabled)
	return out, refused
}

// List renders the catalog for the chat. marked are skills to flag as new.
func (c *Catalog) List(sel Selection, marked []string) string {
	on := c.Enabled(sel)
	var b strings.Builder
	b.WriteString("| Skill | State | Description |\n| --- | --- | --- |\n")
	for _, s := range c.Skills {
		state := "off"
		switch {
		case s.Required:
			state = "required"
		case on[s.Name]:
			state = "on"
		}
		name := "`" + s.Name + "`"
		if slices.Contains(marked, s.Name) {
			name += " (new)"
		}
		fmt.Fprintf(&b, "| %s | %s | %s |\n", name, state, strings.ReplaceAll(s.Description, "|", "/"))
	}
	return b.String()
}

const skillsHelp = "Commands: `/skills` lists the catalog, `/skills off <name>...` and " +
	"`/skills on <name>...` switch optional skills, `/skills reset` returns to the defaults."

func onboardingText(c *Catalog) string {
	return "Your personal Hermes agent has been created. These catalog skills are available:\n\n" +
		c.List(Selection{}, nil) + "\n" + skillsHelp +
		"\n\nSend any other message to start with these defaults."
}

func newSkillsNotice(names []string) string {
	return fmt.Sprintf("New in the skill catalog: %s. Switch any of them off with `/skills off <name>`.\n\n",
		"`"+strings.Join(names, "`, `")+"`")
}

// parseSkillsCommand recognises the reserved `/skills` prefix. ok is false
// for anything else, which then goes to the agent.
func parseSkillsCommand(text string) (verb string, args []string, ok bool) {
	fields := strings.Fields(strings.TrimSpace(text))
	if len(fields) == 0 || fields[0] != "/skills" {
		return "", nil, false
	}
	if len(fields) == 1 {
		return "list", nil, true
	}
	return fields[1], fields[2:], true
}
