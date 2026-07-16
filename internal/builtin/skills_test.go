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
	want := map[string]bool{"SKILL.md": false, "agents/openai.yaml": false, "references/manifest.md": false, "references/integrations.md": false, "references/troubleshooting.md": false}
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
	var skill, troubleshooting string
	for _, file := range snapshot.Files {
		if file.Path == "SKILL.md" {
			skill = string(file.Data)
		}
		if file.Path == "references/troubleshooting.md" {
			troubleshooting = string(file.Data)
		}
	}
	if !strings.Contains(skill, "name: using-ninea") || !strings.Contains(skill, "NineA is a local capability runtime") || !strings.Contains(skill, "9a run <integration>/<capability>") {
		t.Fatalf("invalid SKILL.md: %s", skill)
	}
	for _, text := range []string{"9a status", "9a doctor", "9a doctor --fix", "9a secret set"} {
		if !strings.Contains(troubleshooting, text) {
			t.Fatalf("troubleshooting reference does not contain %q: %s", text, troubleshooting)
		}
	}
	for _, obsolete := range []string{"9a update", "9a attach", "9a project"} {
		if strings.Contains(troubleshooting, obsolete) {
			t.Fatalf("troubleshooting reference contains obsolete command %q: %s", obsolete, troubleshooting)
		}
	}
}

func TestConnectionGuideBootstrapsEveryIntegrationType(t *testing.T) {
	for _, kind := range []string{"http", "mcp", "a2a"} {
		guide, err := ConnectionGuide(kind)
		if err != nil || len(guide) == 0 || !strings.Contains(string(guide), "9a connect") {
			t.Fatalf("ConnectionGuide(%q)=%q, %v", kind, guide, err)
		}
	}
	if _, err := ConnectionGuide("grpc"); err == nil {
		t.Fatal("ConnectionGuide accepted unsupported type")
	}
}
