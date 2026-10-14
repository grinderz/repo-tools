package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grinderz/repo-tools/internal/config"
	"github.com/grinderz/repo-tools/internal/gitx"
	"github.com/grinderz/repo-tools/internal/run"
)

// presetsCtx is a config of the fixture's parent and, as "tooling", its
// submodule repository, which a local> preset can then name; release names
// the branch tooling works on, none for its dev branch.
func presetsCtx(t *testing.T, f fixture, release string) (*run.Ctx, *config.Project) {
	t.Helper()

	return presetsCtxAt(t, f, f.sub, release)
}

// presetsCtxAt is presetsCtx with tooling's clone elsewhere — or nowhere.
func presetsCtxAt(t *testing.T, f fixture, toolingDir, release string) (*run.Ctx, *config.Project) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.yaml")
	body := "projects:\n" +
		"  - name: parent\n" +
		"    git: git@git.example.com:group/parent.git\n" +
		"    project_dir: " + f.parent + "\n" +
		"  - name: tooling\n" +
		"    git: git@git.example.com:group/tooling.git\n" +
		"    project_dir: " + toolingDir + "\n"

	if release != "" {
		body += "    release_branch: " + release + "\n"
	}

	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}

	rctx := testCtx()
	rctx.Cfg = cfg

	return rctx, cfg.Projects[0]
}

const presetFileText = `{
  "$schema": "https://docs.renovatebot.com/renovate-schema.json",
  "extends": [
    "config:recommended",
    "local>group/tooling//renovate/default",
    "local>group/tooling#develop",
    "local>group/tooling//renovate/params(x, y)",
    "local>group/tooling:named",
    "local>other/repo//x#release-9.9",
    "github>group/tooling//renovate/default"
  ]
}
`

// A preset of a configured project follows the branch that project works
// on in this config: a release branch is written as #ref, the dev branch
// as no ref at all; a repository outside the config, and any other preset
// source, is left as written.
func TestRewritePresets(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	rctx, _ := presetsCtx(t, f, "release-1.0")

	out, changes := rewritePresets(rctx, "git.example.com", presetFileText)

	want := strings.NewReplacer(
		`"local>group/tooling//renovate/default"`, `"local>group/tooling//renovate/default#release-1.0"`,
		`"local>group/tooling#develop"`, `"local>group/tooling#release-1.0"`,
		`"local>group/tooling//renovate/params(x, y)"`, `"local>group/tooling//renovate/params#release-1.0(x, y)"`,
		`"local>group/tooling:named"`, `"local>group/tooling:named#release-1.0"`,
	).Replace(presetFileText)
	if out != want {
		t.Errorf("rewritten:\n%s", out)
	}

	// An http port on the project's host is not part of the platform's name.
	if _, withPort := rewritePresets(rctx, "git.example.com:8443", presetFileText); len(withPort) != len(changes) {
		t.Errorf("with a port: %v", withPort)
	}

	lines := make([]string, 0, len(changes))
	for _, c := range changes {
		lines = append(lines, c.String())
	}

	if got := strings.Join(lines, "; "); got != "preset local>group/tooling//renovate/default: "+
		"default branch -> release-1.0; preset local>group/tooling: develop -> release-1.0; "+
		"preset local>group/tooling//renovate/params(x, y): default branch -> release-1.0; "+
		"preset local>group/tooling:named: default branch -> release-1.0" {
		t.Errorf("changes = %s", got)
	}

	// A dev config has tooling on its default branch: a pinned preset is
	// unpinned, which is how a release branch's file reads after a merge
	// back, and a file without pins is not touched at all.
	rctx, _ = presetsCtx(t, f, "")

	out, changes = rewritePresets(rctx, "git.example.com", want)
	if out != strings.ReplaceAll(presetFileText, `"local>group/tooling#develop"`, `"local>group/tooling"`) ||
		len(changes) != 4 || changes[0].To != "" {
		t.Errorf("unpinned:\n%s\n%v", out, changes)
	}

	if out, changes := rewritePresets(rctx, "git.example.com", out); len(changes) != 0 ||
		!strings.Contains(out, `"local>other/repo//x#release-9.9"`) {
		t.Errorf("a settled file must come back as it is: %v\n%s", changes, out)
	}

	// The slug is local to the project's host: on another platform the same
	// group/name is another repository.
	if _, changes := rewritePresets(rctx, "github.com", presetFileText); len(changes) != 0 {
		t.Errorf("another host: %v", changes)
	}
}

// The plan reads the branch being frozen; freezePresets writes the checkout
// in place, layout kept, and a second run finds nothing to do.
//
//nolint:paralleltest // captureOutput swaps os.Stdout
func TestPlanAndFreezePresets(t *testing.T) {
	f := newFixture(t)
	rctx, p := presetsCtx(t, f, "release-1.0")
	r := gitx.Repo{Dir: f.parent}

	plan, err := planPresets(rctx, r, p, "")
	if err != nil || len(plan) != 0 {
		t.Fatalf("without a config file: plan = %v, err = %v", plan, err)
	}

	writeFile(t, f.parent, "renovate.json", presetFileText)
	commit(t, f.parent, "parent: add renovate config")

	plan, err = planPresets(rctx, r, p, "release-1.0")
	if err != nil || len(plan) != 4 || !strings.Contains(plan[0],
		"renovate.json: preset local>group/tooling//renovate/default: default branch -> release-1.0") {
		t.Fatalf("plan = %v, err = %v", plan, err)
	}

	out := captureOutput(t, func() {
		if err := freezePresets(rctx, r, p); err != nil {
			t.Error(err)
		}
	})
	if !strings.Contains(out, "renovate.json: preset local>group/tooling: develop -> release-1.0") {
		t.Errorf("output:\n%s", out)
	}

	body, err := os.ReadFile(filepath.Join(f.parent, "renovate.json"))
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(body), "\n    \"local>group/tooling//renovate/default#release-1.0\",\n") ||
		!strings.Contains(string(body), `"local>other/repo//x#release-9.9"`) {
		t.Errorf("renovate.json:\n%s", body)
	}

	if out := captureOutput(t, func() {
		if err := freezePresets(rctx, r, p); err != nil {
			t.Error(err)
		}
	}); out != "" {
		t.Errorf("a frozen file is nothing to report:\n%s", out)
	}
}

// repo check holds the file on the target branch to the refs this config
// wants: a release branch's preset deps freeze has not pinned, a dev
// branch's preset left pinned.
func TestCheckPresetRefs(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	r := gitx.Repo{Dir: f.parent}

	git(t, f.parent, "checkout", "--quiet", "develop")
	writeFile(t, f.parent, "renovate.json", presetFileText)
	commit(t, f.parent, "parent: add renovate config")

	rctx, p := presetsCtx(t, f, "")

	if issues := checkRenovateConfig(rctx, r, p); len(issues) != 1 || issues[0] !=
		"renovate: renovate.json on develop: preset local>group/tooling pins tooling to develop, "+
			"which follows its default branch (develop) here: drop the ref" {
		t.Errorf("dev config: %v", issues)
	}

	rctx, p = presetsCtx(t, f, "release-1.0")
	p.ReleaseBranch = "release-1.0"

	// renovate: false keeps deps renovate off the project, not the freeze:
	// the file is pinned and held either way.
	off := false
	p.Renovate = &off

	git(t, f.parent, "checkout", "--quiet", "release-1.0")
	writeFile(t, f.parent, "renovate.json", strings.ReplaceAll(presetFileText,
		`"local>group/tooling#develop"`, `"local>group/tooling#release-1.0"`))
	commit(t, f.parent, "parent: half-frozen renovate config")

	if issues := checkRenovateConfig(rctx, r, p); len(issues) != 3 || issues[0] !=
		"renovate: renovate.json on release-1.0: preset local>group/tooling//renovate/default reads "+
			"tooling's default branch, this config has tooling on release-1.0: deps freeze pins it" ||
		!strings.Contains(issues[1], "preset local>group/tooling//renovate/params(x, y) reads") {
		t.Errorf("release config: %v", issues)
	}
}

// A JSON5 file may quote its strings with single quotes, so its refs are
// left alone and said so, in the plan and in the run.
//
//nolint:paralleltest // captureOutput swaps os.Stdout
func TestPresetsInJSON5(t *testing.T) {
	f := newFixture(t)
	rctx, p := presetsCtx(t, f, "release-1.0")
	r := gitx.Repo{Dir: f.parent}

	writeFile(t, f.parent, "renovate.json5", "{extends: ['local>group/tooling']}\n")

	plan, err := planPresets(rctx, r, p, "")
	if err != nil || len(plan) != 1 || plan[0] != "renovate.json5 is JSON5, preset refs not frozen" {
		t.Errorf("plan = %v, err = %v", plan, err)
	}

	out := captureOutput(t, func() {
		if err := freezePresets(rctx, r, p); err != nil {
			t.Error(err)
		}
	})
	if !strings.Contains(out, "renovate.json5 is JSON5, preset refs not frozen") {
		t.Errorf("output:\n%s", out)
	}
}
