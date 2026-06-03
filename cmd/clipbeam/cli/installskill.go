package cli

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
)

// embeddedSkill is the committed SKILL.md, embedded into the binary (PLAN §8.8). It is
// GENERATED from `clipbeam schema` (generateSkillMarkdown) and a CI drift check
// (installskill_test.go) fails the build if it diverges — so the dropped skill can
// never lie.
//
//go:embed skillfs/SKILL.md
var embeddedSkill string

// embeddedAgents is the committed AGENTS.md companion, embedded the same way.
//
//go:embed skillfs/AGENTS.md
var embeddedAgents string

// skillTarget describes one agent config destination (PLAN §8.8).
type skillTarget struct {
	name    string // "claude" | "codex"
	dir     string // the resolved skills/clipbeam dir
	envHint string // the env var that overrides the base config dir
}

// installSkillResult is the `install-skill --json` shape.
type installSkillResult struct {
	Schema  string   `json:"schema"`
	OK      bool     `json:"ok"`
	Written []string `json:"written"`
}

// runInstallSkill implements `clipbeam install-skill` (PLAN §8.8): writes the embedded
// SKILL.md (+ AGENTS.md) to the detected agent config dirs. Idempotent (writes only if
// absent or --force); each written path to stdout, diagnostics to stderr;
// --list-targets enumerates detected dirs without writing.
func runInstallSkill(o out, target, dir string, force, listTargets bool) error {
	targets, err := resolveSkillTargets(target, dir)
	if err != nil {
		return err
	}

	if listTargets {
		for _, t := range targets {
			o.dataln(fmt.Sprintf("%s\t%s", t.name, t.dir))
		}
		return nil
	}

	var written []string
	for _, t := range targets {
		paths, werr := writeSkillFiles(o, t, force)
		if werr != nil {
			return coded(ExitGeneric, werr)
		}
		written = append(written, paths...)
	}

	if o.json {
		return o.emitJSON(installSkillResult{Schema: schemaVersion, OK: true, Written: nonNilStrings(written)})
	}
	for _, p := range written {
		o.dataln(p)
	}
	if len(written) == 0 {
		o.diag("install-skill: nothing written (all targets already present; use --force to overwrite)")
	}
	return nil
}

// resolveSkillTargets resolves the destination dirs for --target (PLAN §8.8),
// respecting $CLAUDE_CONFIG_DIR / $CODEX_HOME. An explicit --dir overrides the base for
// every selected target (the skill is written under <dir>/clipbeam).
func resolveSkillTargets(target, dir string) ([]skillTarget, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil, configError("cannot resolve home directory")
	}

	claudeBase := envOr("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	codexBase := envOr("CODEX_HOME", filepath.Join(home, ".codex"))

	all := map[string]skillTarget{
		"claude": {name: "claude", dir: filepath.Join(claudeBase, "skills", "clipbeam"), envHint: "CLAUDE_CONFIG_DIR"},
		"codex":  {name: "codex", dir: filepath.Join(codexBase, "skills", "clipbeam"), envHint: "CODEX_HOME"},
	}

	// An explicit --dir overrides the base; the skill still lands under <dir>/clipbeam.
	if dir != "" {
		for k, t := range all {
			t.dir = filepath.Join(dir, "clipbeam")
			all[k] = t
		}
	}

	switch target {
	case "all", "":
		return []skillTarget{all["claude"], all["codex"]}, nil
	case "claude":
		return []skillTarget{all["claude"]}, nil
	case "codex":
		return []skillTarget{all["codex"]}, nil
	default:
		return nil, usageError("install-skill: --target must be claude | codex | all (got %q)", target)
	}
}

// writeSkillFiles writes SKILL.md (+ AGENTS.md) into the target dir (PLAN §8.8): dir
// 0755, files 0644, idempotent (skip an existing file unless force). It returns the
// paths actually written.
func writeSkillFiles(o out, t skillTarget, force bool) ([]string, error) {
	if err := os.MkdirAll(t.dir, 0o755); err != nil {
		return nil, fmt.Errorf("create %s: %w", t.dir, err)
	}
	var written []string
	files := []struct {
		name    string
		content string
	}{
		{"SKILL.md", embeddedSkill},
		{"AGENTS.md", embeddedAgents},
	}
	for _, f := range files {
		dest := filepath.Join(t.dir, f.name)
		if !force {
			if _, err := os.Stat(dest); err == nil {
				o.diag("install-skill: %s already exists (use --force to overwrite)", dest)
				continue
			}
		}
		if err := os.WriteFile(dest, []byte(f.content), 0o644); err != nil {
			return written, fmt.Errorf("write %s: %w", dest, err)
		}
		written = append(written, dest)
	}
	return written, nil
}

// envOr returns $name if set and non-empty, else def.
func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// nonNilStrings returns s, or an empty non-nil slice, so the JSON "written" field is
// [] rather than null when nothing was written.
func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
