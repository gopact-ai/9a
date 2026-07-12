package builtin

import (
	"strings"
	"testing"
)

func TestUsingNineASkillBundle(t *testing.T) {
	snapshot, err := UsingNineA("test-version")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Name != "using-ninea" || snapshot.LogicalID != "builtin/using-ninea" || snapshot.Digest == "" {
		t.Fatalf("snapshot=%#v", snapshot)
	}
	want := map[string]bool{"SKILL.md": false, "agents/openai.yaml": false, "references/declarative.md": false, "references/integrations.md": false, "references/troubleshooting.md": false}
	for _, file := range snapshot.Files {
		if _, ok := want[file.Path]; !ok {
			t.Fatalf("unexpected file %q", file.Path)
		}
		want[file.Path] = true
		if strings.Contains(string(file.Data), "TODO") {
			t.Fatalf("placeholder in %s", file.Path)
		}
	}
	for path, found := range want {
		if !found {
			t.Fatalf("missing %s", path)
		}
	}
	var skill string
	for _, file := range snapshot.Files {
		if file.Path == "SKILL.md" {
			skill = string(file.Data)
		}
	}
	if !strings.Contains(skill, "name: using-ninea") || !strings.Contains(skill, "Use when an AI agent needs") {
		t.Fatalf("invalid SKILL.md: %s", skill)
	}
}
