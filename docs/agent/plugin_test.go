// SPDX-License-Identifier: AGPL-3.0-only

package agentdocs

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

const pluginSkillsDir = "../../claude-plugin/skills"

type skillFrontmatter struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

var guideCall = regexp.MustCompile("`dns_guide`" + ` tool with ` + "`skill: \"([a-z0-9-]+)\"`")

func TestPluginSkillsCoverEmbeddedSkills(t *testing.T) {
	entries, err := fs.ReadDir(FS, SkillsDir)
	if err != nil {
		t.Fatalf("reading embedded skills: %v", err)
	}
	embedded := map[string]bool{}
	for _, entry := range entries {
		embedded[strings.TrimSuffix(entry.Name(), ".md")] = true
	}
	if len(embedded) == 0 {
		t.Fatalf("no skills embedded from %s", SkillsDir)
	}

	dirs, err := os.ReadDir(pluginSkillsDir)
	if err != nil {
		t.Fatalf("reading %s: %v", pluginSkillsDir, err)
	}
	wrapped := map[string]bool{}
	for _, dir := range dirs {
		raw, err := os.ReadFile(filepath.Join(pluginSkillsDir, dir.Name(), "SKILL.md"))
		if err != nil {
			continue
		}
		m := guideCall.FindStringSubmatch(string(raw))
		if m == nil {
			continue
		}
		if m[1] != dir.Name() {
			t.Errorf("claude-plugin/skills/%s asks dns_guide for %q; the names must match", dir.Name(), m[1])
		}
		if !embedded[m[1]] {
			t.Errorf("claude-plugin/skills/%s asks dns_guide for %q, which docs/agent/skills does not publish",
				dir.Name(), m[1])
		}
		wrapped[m[1]] = true
	}

	for name := range embedded {
		if !wrapped[name] {
			t.Errorf("docs/agent/skills/%s.md has no Claude Code plugin skill; "+
				"add claude-plugin/skills/%s/SKILL.md that calls dns_guide", name, name)
		}
	}
}

func TestPluginSkillsHaveFrontmatter(t *testing.T) {
	dirs, err := os.ReadDir(pluginSkillsDir)
	if err != nil {
		t.Fatalf("reading %s: %v", pluginSkillsDir, err)
	}
	if len(dirs) == 0 {
		t.Fatalf("no skills in %s", pluginSkillsDir)
	}

	for _, dir := range dirs {
		if !dir.IsDir() {
			continue
		}
		t.Run(dir.Name(), func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(pluginSkillsDir, dir.Name(), "SKILL.md"))
			if err != nil {
				t.Fatalf("reading SKILL.md: %v", err)
			}
			rest, ok := strings.CutPrefix(string(raw), "---\n")
			if !ok {
				t.Fatalf("SKILL.md does not open with frontmatter")
			}
			header, _, ok := strings.Cut(rest, "\n---\n")
			if !ok {
				t.Fatalf("SKILL.md frontmatter is not closed")
			}

			var fm skillFrontmatter
			if err := yaml.Unmarshal([]byte(header), &fm); err != nil {
				t.Fatalf("parsing frontmatter: %v", err)
			}
			if fm.Name != dir.Name() {
				t.Errorf("frontmatter name %q does not match directory %q", fm.Name, dir.Name())
			}
			if strings.TrimSpace(fm.Description) == "" {
				t.Errorf("frontmatter has no description")
			}
		})
	}
}
