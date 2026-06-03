package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSkillDoesNotDrift is the CI drift gate (PLAN §8.8): the committed embedded
// SKILL.md / AGENTS.md MUST equal the output of generateSkillMarkdown /
// generateAgentsMarkdown over the current schema, so the dropped skill can never lie.
//
// To REGENERATE after an intentional schema change, run with CLIPBEAM_REGEN_SKILL=1:
//
//	CLIPBEAM_REGEN_SKILL=1 go test ./cmd/clipbeam/cli -run TestSkillDoesNotDrift
//
// which rewrites skillfs/SKILL.md + skillfs/AGENTS.md from the generator, then the
// plain test run passes.
func TestSkillDoesNotDrift(t *testing.T) {
	// The version is intentionally excluded from the skill body (a version bump must not
	// force a skill rewrite), so any fixed version produces the canonical document.
	doc := buildSchema("dev")
	wantSkill := generateSkillMarkdown(doc)
	wantAgents := generateAgentsMarkdown(doc)

	if os.Getenv("CLIPBEAM_REGEN_SKILL") == "1" {
		writeGen(t, "skillfs/SKILL.md", wantSkill)
		writeGen(t, "skillfs/AGENTS.md", wantAgents)
		t.Log("regenerated skillfs/SKILL.md and skillfs/AGENTS.md")
		return
	}

	if embeddedSkill != wantSkill {
		t.Errorf("embedded SKILL.md has drifted from `clipbeam schema` output.\n"+
			"Run: CLIPBEAM_REGEN_SKILL=1 go test ./cmd/clipbeam/cli -run TestSkillDoesNotDrift\n"+
			"got %d bytes, want %d bytes", len(embeddedSkill), len(wantSkill))
	}
	if embeddedAgents != wantAgents {
		t.Errorf("embedded AGENTS.md has drifted from `clipbeam schema` output.\n"+
			"Run: CLIPBEAM_REGEN_SKILL=1 go test ./cmd/clipbeam/cli -run TestSkillDoesNotDrift\n"+
			"got %d bytes, want %d bytes", len(embeddedAgents), len(wantAgents))
	}
}

// writeGen writes generated content to a file path relative to the package dir.
func writeGen(t *testing.T, rel, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Clean(rel), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}
