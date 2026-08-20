package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grinderz/repo-tools/internal/config"
	"github.com/grinderz/repo-tools/internal/gitx"
)

// A pin at the head of its branch is in step; after the branch moves, the
// same pin is behind by exactly the commits it is missing.
func TestDriftAgainst(t *testing.T) {
	f := newFixture(t)
	p := f.project(t)
	sub := gitx.Repo{Dir: filepath.Join(f.parent, "sub")}

	git(t, f.parent, "checkout", "--quiet", "release-1.0")
	git(t, f.parent, "submodule", "update", "--quiet", "--", "sub")

	got := driftAgainst(testCtx(), p, sub, f.subRelease, "release-1.0", "")
	if got.Drifted || !strings.Contains(got.Line, "ok") {
		t.Errorf("pin at head: %+v", got)
	}

	// The submodule's branch moves on; the clone learns of it via fetch.
	git(t, f.sub, "checkout", "--quiet", "release-1.0")
	writeFile(t, f.sub, "lib.txt", "v1-hotfix-2\n")
	commit(t, f.sub, "sub: second hotfix")
	git(t, sub.Dir, "fetch", "--quiet", "origin")

	got = driftAgainst(testCtx(), p, sub, f.subRelease, "release-1.0", "")
	if !got.Drifted || !strings.Contains(got.Line, "behind 1 commit(s)") {
		t.Errorf("pin one commit back: %+v", got)
	}

	got = driftAgainst(testCtx(), p, sub, f.subRelease, "nope", "")
	if !got.Drifted || !strings.Contains(got.Line, "does not exist") {
		t.Errorf("unknown branch: %+v", got)
	}
}

// The submodule report reads the pins of the target ref, not the checkout.
func TestSubmodulePinLines(t *testing.T) {
	f := newFixture(t)
	p := f.project(t)
	r := gitx.Repo{Dir: f.parent}

	git(t, f.parent, "checkout", "--quiet", "release-1.0")
	git(t, f.parent, "submodule", "update", "--quiet", "--", "sub")
	git(t, f.parent, "update-ref", "refs/remotes/origin/release-1.0", "release-1.0")

	lines := submodulePinLines(testCtx(), p, r, "origin/release-1.0")
	if len(lines) != 1 || lines[0].Drifted || !strings.Contains(lines[0].Line, "submodule sub") {
		t.Errorf("lines = %+v", lines)
	}
}

func TestGoModVersion(t *testing.T) {
	gomod := "module example.com/app\n\nrequire (\n\texample.com/lib v1.2.3\n" +
		"\texample.com/lib2 v0.0.0-20260810120000-abcdef123456\n)\n"

	if got := goModVersion(gomod, "example.com/lib"); got != "v1.2.3" {
		t.Errorf("tag version: %q", got)
	}

	if got := goModVersion(gomod, "example.com/lib2"); got != "v0.0.0-20260810120000-abcdef123456" {
		t.Errorf("pseudo version: %q", got)
	}

	if got := goModVersion(gomod, "example.com/other"); got != "" {
		t.Errorf("absent module: %q", got)
	}
}

// A pseudo-version names its commit outright; a released version goes through
// the tag, spelled with or without the leading v.
func TestVersionCommit(t *testing.T) {
	f := newFixture(t)
	r := gitx.Repo{Dir: f.parent}
	head := git(t, f.parent, "rev-parse", "HEAD")
	git(t, f.parent, "tag", "1.2.3")

	sha, err := versionCommit(r, "v0.0.0-20260821000000-"+head[:12])
	if err != nil || sha != head {
		t.Errorf("pseudo: sha=%s err=%v", sha, err)
	}

	sha, err = versionCommit(r, "v1.2.3")
	if err != nil || sha != head {
		t.Errorf("tag: sha=%s err=%v", sha, err)
	}
}

// The module report ties the Makefile pin, go.mod and the provider's clone
// together: a pin on the config's branch with go.mod at its head is ok, a pin
// on another branch is called out.
func TestModulePinLines(t *testing.T) {
	f := newFixture(t)
	r := gitx.Repo{Dir: f.parent}

	git(t, f.parent, "checkout", "--quiet", "develop")
	writeFile(t, f.parent, "Makefile", "GO_DEPS_UPDATE_INTERNAL = "+f.sub+"@develop\n")
	writeFile(t, f.parent, "go.mod",
		"module example.com/app\n\nrequire "+f.sub+" v0.0.0-20260821000000-"+f.subDevelop[:12]+"\n")
	commit(t, f.parent, "parent: pin files")
	git(t, f.parent, "update-ref", "refs/remotes/origin/develop", "develop")

	path := filepath.Join(t.TempDir(), "config.yaml")
	body := "deps_cmds:\n  go: [\"go mod tidy\"]\n" +
		"deps_pins:\n  go: {file: Makefile, var: GO_DEPS_UPDATE_INTERNAL}\n" +
		"projects:\n" +
		"  - {name: parent, git: " + f.parent + ", project_dir: " + f.parent + ", deps: go}\n" +
		"  - {name: lib, git: " + f.sub + ", project_dir: " + f.sub + "}\n"

	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}

	rctx := testCtx()
	rctx.Cfg = cfg
	rctx.Cfg.Confirm = config.ConfirmNever

	// The provider's clone answers for origin/develop.
	git(t, f.sub, "update-ref", "refs/remotes/origin/develop", "develop")

	lines := modulePinLines(rctx, r, cfg.Projects[0], "origin/develop", map[string]bool{"lib": true})
	if len(lines) != 1 || lines[0].Drifted || !strings.Contains(lines[0].Line, "ok") {
		t.Errorf("pin at the dev head: %+v", lines)
	}

	// A release config would pin the release branch; the dev pin is stale.
	cfg.Projects[1].ReleaseBranch = "release-1.0"

	lines = modulePinLines(rctx, r, cfg.Projects[0], "origin/develop", map[string]bool{"lib": true})
	if len(lines) != 1 || !lines[0].Drifted || !strings.Contains(lines[0].Line, "config would pin release-1.0") {
		t.Errorf("stale pin branch: %+v", lines)
	}
}

// deps check runs the kind's commands and folds their exit codes into one
// verdict per project.
func TestDepsCheckProject(t *testing.T) {
	f := newFixture(t)
	p := f.project(t)
	p.Deps = "go"

	rctx := testCtx()
	rctx.Cfg.DepsCmds = map[string][]string{"go": {"true"}}
	rctx.Cfg.DepsCheckCmds = map[string][]string{"go": {"true"}}

	if ok, ran := depsCheckProject(rctx, p); !ok || !ran {
		t.Errorf("passing check: ok=%v ran=%v", ok, ran)
	}

	rctx.Cfg.DepsCheckCmds["go"] = []string{"false"}

	if ok, ran := depsCheckProject(rctx, p); ok || !ran {
		t.Errorf("failing check: ok=%v ran=%v", ok, ran)
	}

	rctx.Cfg.DepsCheckCmds = nil

	if _, ran := depsCheckProject(rctx, p); ran {
		t.Error("no checks configured must not count as a run")
	}
}
