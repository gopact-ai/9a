package declarative

import (
	"io/fs"
	"strings"
	"testing"
)

func TestRenderSkillGroupsOperationsAndWorkflows(t *testing.T) {
	config, err := Parse([]byte(validSource))
	if err != nil {
		t.Fatal(err)
	}
	skill, err := RenderSkill(config)
	if err != nil {
		t.Fatal(err)
	}
	if skill.Name != "weather" || skill.CapabilityID != "api/weather" {
		t.Fatalf("skill=%#v", skill)
	}
	want := map[string]fs.FileMode{
		"SKILL.md":                       0o644,
		"operations/current/schema.json": 0o644,
		"operations/current/invoke":      0o755,
		"workflows/report/schema.json":   0o644,
		"workflows/report/invoke":        0o755,
		"references/source.yaml":         0o600,
	}
	for _, file := range skill.Files {
		mode, ok := want[file.Path]
		if !ok {
			t.Fatalf("unexpected file %q", file.Path)
		}
		if file.Mode != mode {
			t.Fatalf("%s mode=%o", file.Path, file.Mode)
		}
		if file.Path == "SKILL.md" && (!strings.Contains(string(file.Data), "current") || !strings.Contains(string(file.Data), "report")) {
			t.Fatalf("SKILL.md=%s", file.Data)
		}
		delete(want, file.Path)
	}
	if len(want) != 0 {
		t.Fatalf("missing files=%v", want)
	}
}
