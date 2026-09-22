package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grinderz/repo-tools/internal/gitx"
)

// A clean tree is planned as a skip, a dirty one names what would go.
func TestPlanCleanCountsTheChanges(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	p := f.project(t)

	step := planClean(testCtx(), p, false)
	if !step.Skip || !strings.Contains(step.Warn, "already clean") {
		t.Fatalf("skip=%v warn=%q", step.Skip, step.Warn)
	}

	writeFile(t, f.parent, "app.txt", "local edit\n")

	step = planClean(testCtx(), p, false)
	if step.Skip {
		t.Fatalf("a dirty tree is work to do: %q", step.Warn)
	}

	plan := strings.Join(step.Plan, "\n")
	if !strings.Contains(plan, "discard 1 change(s): M app.txt") {
		t.Errorf("plan = %q", plan)
	}
}

// Untracked files are left alone unless asked for, since deleting them is the
// one thing here git itself cannot undo.
func TestPlanCleanLeavesUntrackedFilesAlone(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	p := f.project(t)
	writeFile(t, f.parent, "scratch.txt", "not tracked\n")

	if step := planClean(testCtx(), p, false); !step.Skip {
		t.Errorf("an untracked file is not a reason to clean: %v", step.Plan)
	}

	step := planClean(testCtx(), p, true)
	if step.Skip || !strings.Contains(strings.Join(step.Plan, "\n"), "scratch.txt") {
		t.Errorf("--untracked should pick it up: %v %q", step.Plan, step.Warn)
	}
}

// The whole point: a tree the other commands refuse to touch becomes usable
// again, submodule pins included.
//
//nolint:paralleltest // captures os.Stdout, which is process-wide
func TestCleanProjectRestoresTheTree(t *testing.T) {
	f := newFixture(t)
	p := f.project(t)
	r := gitx.Repo{Dir: f.parent}

	pinBefore := subPin(t, f.parent)

	writeFile(t, f.parent, "app.txt", "half-done work\n")
	git(t, f.parent, "-C", "sub", "checkout", "--quiet", "develop")
	writeFile(t, f.parent, "scratch.txt", "not tracked\n")

	if err := requireClean(r); err == nil {
		t.Fatal("the fixture should be dirty before the clean")
	}

	out := captureOutput(t, func() {
		if err := cleanProject(testCtx(), p, false); err != nil {
			t.Errorf("clean failed: %v", err)
		}
	})

	if !strings.Contains(out, "is about to discard") {
		t.Errorf("the changes should be shown first:\n%s", out)
	}

	// Tracked changes are gone; the untracked file is still there, and
	// requireClean counts it, which is why --untracked exists.
	if tracked := git(t, f.parent, "status", "--porcelain", "--untracked-files=no"); tracked != "" {
		t.Errorf("tracked changes should be gone:\n%s", tracked)
	}

	if got := subPin(t, f.parent); got != pinBefore {
		t.Errorf("submodule pin = %s, want the recorded %s", got, pinBefore)
	}

	// Untracked files survive without --untracked.
	if _, err := os.Stat(filepath.Join(f.parent, "scratch.txt")); err != nil {
		t.Errorf("an untracked file must not be deleted: %v", err)
	}

	if err := cleanProject(testCtx(), p, true); err != nil {
		t.Fatalf("clean --untracked failed: %v", err)
	}

	if err := requireClean(r); err != nil {
		t.Errorf("after --untracked nothing should be left: %v", err)
	}

	if _, err := os.Stat(filepath.Join(f.parent, "scratch.txt")); err == nil {
		t.Error("--untracked should have deleted it")
	}
}

// A project that keeps its GOPROXY or index URLs in .envrc must have them when
// its own commands run, or the build reaches for the public proxy instead.
func TestDirenvStateAndShell(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	r := gitx.Repo{Dir: f.parent}
	rctx := testCtx()

	if use, _ := direnvState(rctx, r); use {
		t.Error("without an .envrc there is nothing to load")
	}

	writeFile(t, f.parent, ".envrc", "export RT_DIRENV_PROBE=loaded\n")

	use, blocked := direnvState(rctx, r)
	if !use {
		t.Skip("direnv is not installed here")
	}

	if !blocked {
		t.Fatal("a fresh .envrc is blocked until direnv allow, and that must be visible")
	}

	if plan := strings.Join(direnvPlanLines(rctx, r), " "); !strings.Contains(plan, "direnv allow") {
		t.Errorf("the plan should say what to do about it: %q", plan)
	}

	// Turning it off in the config takes the whole thing out of the way.
	off := false
	rctx.Cfg.Direnv = &off

	if use, _ := direnvState(rctx, r); use {
		t.Error("direnv: false should disable it")
	}
}
