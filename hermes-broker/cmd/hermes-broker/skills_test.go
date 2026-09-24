package main

import (
	"slices"
	"strings"
	"testing"
)

func testCatalog() *Catalog {
	return &Catalog{Version: "v1", Skills: []CatalogSkill{
		{Name: "change-summary", Description: "summaries", DefaultEnabled: false},
		{Name: "platform-guide", Description: "guide", Required: true, DefaultEnabled: true},
		{Name: "repo-reader", Description: "reader", DefaultEnabled: true},
	}}
}

func TestApplySelection(t *testing.T) {
	c := testCatalog()
	sel, refused := c.Apply(Selection{}, false, []string{"repo-reader", "platform-guide", "nope"})
	if !slices.Equal(sel.Disabled, []string{"repo-reader"}) || len(refused) != 2 {
		t.Fatalf("off: %+v %v", sel, refused)
	}
	sel, _ = c.Apply(sel, true, []string{"change-summary", "repo-reader"})
	if len(sel.Disabled) != 0 || !slices.Equal(sel.Enabled, []string{"change-summary"}) {
		t.Fatalf("on: %+v", sel)
	}
	on := c.Enabled(sel)
	if !on["change-summary"] || !on["platform-guide"] || !on["repo-reader"] {
		t.Fatalf("enabled: %v", on)
	}
}

func TestParseSkillsCommand(t *testing.T) {
	for in, want := range map[string]string{"/skills": "list", " /skills off a b": "off", "/skills reset": "reset"} {
		if verb, _, ok := parseSkillsCommand(in); !ok || verb != want {
			t.Errorf("%q: %q %v", in, verb, ok)
		}
	}
	for _, in := range []string{"/skillsx", "tell me about /skills", "hello"} {
		if _, _, ok := parseSkillsCommand(in); ok {
			t.Errorf("%q parsed as a command", in)
		}
	}
}

func TestListMarksState(t *testing.T) {
	out := testCatalog().List(Selection{Disabled: []string{"repo-reader"}}, []string{"change-summary"})
	for _, want := range []string{"`change-summary` (new) | off", "`platform-guide` | required", "`repo-reader` | off"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
}
