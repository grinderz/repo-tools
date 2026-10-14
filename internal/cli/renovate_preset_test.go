package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writePreset puts a preset file into a working copy.
func writePreset(t *testing.T, root, rel, content string) {
	t.Helper()

	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// dirReader is a presetReader over working copies by slug, as
// projectPresetReader reads a preset without a ref.
func dirReader(roots map[string]string) presetReader {
	return func(slug, _, file string) ([]byte, string, bool, string) {
		root, ok := roots[slug]
		if !ok {
			return nil, "", false, ""
		}

		where := filepath.Base(root) + "'s working copy"

		data, err := os.ReadFile(filepath.Join(root, file))
		if err != nil {
			return nil, where, true, filepath.Base(file) + " not in " + where
		}

		return data, where, true, ""
	}
}

// A local> preset of a project in the config comes from its working copy:
// its keys under the file's own, the rules the preset's first, its own
// extends in its place; one of a repository outside the config stays.
func TestRenovateInlinePresets(t *testing.T) {
	t.Parallel()

	tooling := filepath.Join(t.TempDir(), "tooling")

	writePreset(t, tooling, "renovate/default.json", `{"$schema":"x","description":["shared"],
		"extends":["config:recommended","local>group/tooling//renovate/base"],
		"enabledManagers":["gomod"],"labels":["shared"],
		"vulnerabilityAlerts":{"enabled":true,"labels":["security"]},
		"packageRules":[{"description":"shared rule"}]}`)
	writePreset(t, tooling, "renovate/base.json",
		`{"commitMessagePrefix":"build:","packageRules":[{"description":"base rule"}]}`)
	writePreset(t, tooling, "default.json", `{"timezone":"UTC","postUpdateOptions":["gomodTidy"]}`)
	writePreset(t, tooling, "level.json", `{"separateMinorPatch":true}`)
	writePreset(t, tooling, "named.json", `{"ignoreDeps":["left-pad"]}`)

	dir := dirReader(map[string]string{"group/tooling": tooling})

	var cfg map[string]any
	if err := json.Unmarshal([]byte(`{"extends":["local>other/repo//x","local>group/tooling//renovate/default#develop",
		"local>group/tooling","local>group/tooling:named"],"labels":["own"],
		"vulnerabilityAlerts":{"labels":["own-security"]},"postUpdateOptions":["gomodUpdateImportPaths"],
		"packageRules":[{"description":"own rule"}]}`), &cfg); err != nil {
		t.Fatal(err)
	}

	taken, reason := renovateInlinePresets(cfg, dir, nil)
	if reason != "" {
		t.Fatal(reason)
	}

	if strings.Join(taken, "; ") != "renovate/default.json from tooling's working copy; "+
		"renovate/base.json from tooling's working copy; default.json from tooling's working copy; "+
		"named.json from tooling's working copy" {
		t.Errorf("taken = %v", taken)
	}

	var buf strings.Builder

	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)

	if err := enc.Encode(cfg); err != nil {
		t.Fatal(err)
	}

	out := buf.String()
	for _, want := range []string{
		`"extends":["local>other/repo//x","config:recommended"]`,
		`"labels":["own"]`,
		`"enabledManagers":["gomod"]`,
		`"timezone":"UTC"`,
		`"postUpdateOptions":["gomodTidy","gomodUpdateImportPaths"]`, // a mergeable list: both
		`"ignoreDeps":["left-pad"]`,
		`"commitMessagePrefix":"build:"`,
		`"vulnerabilityAlerts":{"enabled":true,"labels":["own-security"]}`,
		`{"description":"base rule"},{"description":"shared rule"},{"description":"own rule"}`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("merged config lacks %s:\n%s", want, out)
		}
	}

	if strings.Contains(out, `"shared"]`) || strings.Contains(out, "$schema") {
		t.Errorf("the preset's description and schema must go:\n%s", out)
	}

	// A preset's own level key is caught once it is in the file.
	leveled := map[string]any{"extends": []any{"local>group/tooling//level"}}
	if _, reason := renovateInlinePresets(leveled, dir, nil); reason != "" {
		t.Fatal(reason)
	}

	if key, hint := renovateRefusedKey(leveled); key != "separateMinorPatch" || hint != "which --update owns" {
		t.Errorf("a level key from a preset: %q %q", key, hint)
	}

	// No extends at all is no work.
	plain := map[string]any{"labels": []any{"x"}}
	if taken, reason := renovateInlinePresets(plain, dir, nil); len(taken) != 0 || reason != "" ||
		plain["extends"] != nil {
		t.Errorf("plain config: %v %q %v", taken, reason, plain)
	}
}

// Two presets side by side resolve as Renovate does: the later one's keys
// win, the rules come in extends order, the file's own last.
func TestRenovateInlinePresetsSiblings(t *testing.T) {
	t.Parallel()

	tooling := filepath.Join(t.TempDir(), "tooling")
	writePreset(t, tooling, "a.json",
		`{"labels":["A"],"timezone":"UTC","packageRules":[{"description":"a"}],"hostRules":[{"matchHost":"a"}]}`)
	writePreset(t, tooling, "b.json", `{"labels":["B"],"packageRules":[{"description":"b"}]}`)

	dir := dirReader(map[string]string{"g/t": tooling})

	var cfg map[string]any
	if err := json.Unmarshal([]byte(`{"extends":["local>g/t//a","local>g/t//b"],
		"packageRules":[{"description":"own"}],"hostRules":[{"matchHost":"own"}]}`), &cfg); err != nil {
		t.Fatal(err)
	}

	if _, reason := renovateInlinePresets(cfg, dir, nil); reason != "" {
		t.Fatal(reason)
	}

	out, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{
		`"labels":["B"]`, `"timezone":"UTC"`,
		`"packageRules":[{"description":"a"},{"description":"b"},{"description":"own"}]`,
		`"hostRules":[{"matchHost":"a"},{"matchHost":"own"}]`,
	} {
		if !strings.Contains(string(out), want) {
			t.Errorf("merged config lacks %s:\n%s", want, out)
		}
	}

	if _, has := cfg["extends"]; has {
		t.Errorf("both presets taken, nothing left to extend: %v", cfg["extends"])
	}
}

// A preset that is not there, or JSON5, or that extends itself, is a
// reason the project is skipped with.
func TestRenovateInlinePresetsRefuses(t *testing.T) {
	t.Parallel()

	tooling := filepath.Join(t.TempDir(), "tooling")
	writePreset(t, tooling, "renovate/loop.json", `{"extends":["local>group/tooling//renovate/loop"]}`)
	writePreset(t, tooling, "renovate/odd.json", `{"packageRules":{"enabled":false}}`)

	dir := dirReader(map[string]string{"group/tooling": tooling})

	for _, tc := range []struct{ extends, want string }{
		{`"local>group/tooling//renovate/missing"`, "missing.json not in tooling's working copy"},
		{`"local>group/tooling//renovate/default.json5"`, "a JSON5 preset is Renovate's own to read"},
		{`"local>group/tooling//renovate/loop"`, "extends itself"},
		{`"local>group/tooling//renovate/odd"`, "odd.json: packageRules is not a list"},
	} {
		var bad map[string]any
		if err := json.Unmarshal([]byte(`{"extends":[`+tc.extends+`]}`), &bad); err != nil {
			t.Fatal(err)
		}

		if _, reason := renovateInlinePresets(bad, dir, nil); !strings.Contains(reason, tc.want) {
			t.Errorf("%s: reason = %q, want %q", tc.extends, reason, tc.want)
		}
	}
}

// A #ref on a preset reads that branch of the project — the local branch
// first, so a commit not yet pushed counts as a working copy does, origin's
// when there is no local one — and a preset without a ref its working
// copy; a project not cloned is ours but unreadable, a project of another
// host is not the one named.
func TestProjectPresetReader(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	rctx, _ := presetsCtx(t, f, "")

	// origin has every branch; locally release-1.0 moves on and next goes.
	bare := filepath.Join(t.TempDir(), "sub.git")
	git(t, f.sub, "clone", "--bare", "--quiet", f.sub, bare)
	git(t, f.sub, "remote", "add", "origin", bare)
	git(t, f.sub, "fetch", "--quiet", "origin")
	git(t, f.sub, "branch", "--quiet", "-D", "next")
	git(t, f.sub, "checkout", "--quiet", "release-1.0")
	writeFile(t, f.sub, "lib.txt", "v1-hotfix-2\n")
	commit(t, f.sub, "sub: unpushed hotfix")
	git(t, f.sub, "checkout", "--quiet", "develop")
	writeFile(t, f.sub, "lib.txt", "uncommitted\n")

	read := projectPresetReader(rctx.Cfg.Projects, "git.example.com")

	for _, tc := range []struct {
		ref, want, where, reason string
	}{
		{ref: "", want: "uncommitted\n", where: "tooling's working copy"},
		{ref: "release-1.0", want: "v1-hotfix-2", where: "tooling on release-1.0"}, // git show trims the newline
		{ref: "next", want: "v2", where: "tooling on next"},
		{ref: "nowhere", where: "tooling on nowhere", reason: "lib.txt not in tooling on nowhere"},
	} {
		data, where, ok, reason := read("group/tooling", tc.ref, "lib.txt")
		if string(data) != tc.want || where != tc.where || !ok || reason != tc.reason {
			t.Errorf("ref %q: %q %q %v %q", tc.ref, data, where, ok, reason)
		}
	}

	if _, _, ok, _ := read("group/other", "", "lib.txt"); ok {
		t.Error("a repository outside the config is not ours")
	}

	if _, _, ok, _ := projectPresetReader(rctx.Cfg.Projects, "github.com")("group/tooling", "", "lib.txt"); ok {
		t.Error("the same slug on another host is another repository")
	}

	gone := filepath.Join(t.TempDir(), "gone")
	rctx, _ = presetsCtxAt(t, f, gone, "")

	if _, _, ok, reason := projectPresetReader(rctx.Cfg.Projects, "git.example.com")("group/tooling", "", "x"); !ok ||
		!strings.HasPrefix(reason, "tooling not cloned at "+gone) {
		t.Errorf("a project not cloned: ok=%v %q", ok, reason)
	}
}
