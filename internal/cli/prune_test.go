package cli

import (
	"strings"
	"testing"

	"github.com/grinderz/repo-tools/internal/config"
	"github.com/grinderz/repo-tools/internal/gitx"
)

// A local release branch in step with origin is a leftover; one with its own
// commits is somebody's work and stays.
func TestPruneTargetsReleaseLeftover(t *testing.T) {
	f := newFixture(t)
	p := f.project(t)
	r := gitx.Repo{Dir: f.parent}

	git(t, f.parent, "checkout", "--quiet", "develop")
	git(t, f.parent, "update-ref", "refs/remotes/origin/develop", "develop")
	git(t, f.parent, "update-ref", "refs/remotes/origin/release-1.0", "release-1.0")

	targets, kept, err := pruneTargets(r, p)
	if err != nil {
		t.Fatal(err)
	}

	if len(targets) != 1 || targets[0].Name != "release-1.0" || len(kept) != 0 {
		t.Errorf("targets = %v, kept = %v", targets, kept)
	}

	// The same branch with a commit of its own must be kept and named.
	git(t, f.parent, "checkout", "--quiet", "release-1.0")
	writeFile(t, f.parent, "local.txt", "work\n")
	commit(t, f.parent, "parent: unpushed work")
	git(t, f.parent, "checkout", "--quiet", "develop")

	targets, kept, err = pruneTargets(r, p)
	if err != nil {
		t.Fatal(err)
	}

	if len(targets) != 0 || len(kept) != 1 || !strings.Contains(kept[0], "inspect it first") {
		t.Errorf("targets = %v, kept = %v", targets, kept)
	}
}

// A branch whose upstream vanished from origin is deleted only when origin
// still carries its commits some other way.
func TestPruneTargetsGoneUpstream(t *testing.T) {
	f := newFixture(t)
	p := f.project(t)
	r := gitx.Repo{Dir: f.parent}

	git(t, f.parent, "checkout", "--quiet", "develop")
	git(t, f.parent, "update-ref", "refs/remotes/origin/develop", "develop")

	// feature sat on origin once; its ref is gone but its commits are all on
	// origin/develop. The upstream is written by hand: the fixture's parent
	// has no real remote to have tracked it through.
	git(t, f.parent, "remote", "add", "origin", f.sub)
	git(t, f.parent, "branch", "feature", "develop")
	git(t, f.parent, "config", "branch.feature.remote", "origin")
	git(t, f.parent, "config", "branch.feature.merge", "refs/heads/feature")

	targets, kept, err := pruneTargets(r, p)
	if err != nil {
		t.Fatal(err)
	}

	found := false

	for _, target := range targets {
		if target.Name == "feature" {
			found = true

			if !strings.Contains(target.Reason, "upstream is gone") {
				t.Errorf("reason = %q", target.Reason)
			}
		}
	}

	if !found {
		t.Errorf("feature not pruned: targets = %v, kept = %v", targets, kept)
	}

	// With a commit of its own the branch is the only copy of that work.
	git(t, f.parent, "checkout", "--quiet", "feature")
	writeFile(t, f.parent, "only-here.txt", "x\n")
	commit(t, f.parent, "parent: only on feature")
	git(t, f.parent, "checkout", "--quiet", "develop")

	targets, kept, err = pruneTargets(r, p)
	if err != nil {
		t.Fatal(err)
	}

	for _, target := range targets {
		if target.Name == "feature" {
			t.Errorf("a branch with unique commits must not be pruned: %v", targets)
		}
	}

	if len(kept) == 0 || !strings.Contains(strings.Join(kept, ";"), "no remote branch carries") {
		t.Errorf("kept = %v", kept)
	}
}

// pruneBranches deletes exactly what the plan named.
func TestPruneBranches(t *testing.T) {
	f := newFixture(t)
	r := gitx.Repo{Dir: f.parent}

	git(t, f.parent, "checkout", "--quiet", "develop")

	if err := pruneBranches(r, []pruneTarget{{Name: "release-1.0", Reason: "test"}}); err != nil {
		t.Fatal(err)
	}

	if r.LocalBranchExists("release-1.0") {
		t.Error("release-1.0 still exists")
	}
}

// exec builds one step per project and actually runs the command.
func TestExecSteps(t *testing.T) {
	f := newFixture(t)
	p := f.project(t)

	steps := execSteps(testCtx(), []*config.Project{p}, "true")
	if len(steps) != 1 || steps[0].Skip {
		t.Fatalf("steps = %+v", steps)
	}

	if !strings.Contains(steps[0].Plan[0], "run: true") {
		t.Errorf("plan = %v", steps[0].Plan)
	}

	// The message placeholders expand per project before anything runs.
	expanded := execSteps(testCtx(), []*config.Project{p}, "echo {project} on {branch}")
	if !strings.Contains(expanded[0].Plan[0], "echo parent on develop") {
		t.Errorf("plan = %v", expanded[0].Plan)
	}

	if err := steps[0].Exec(); err != nil {
		t.Errorf("true failed: %v", err)
	}

	if err := execSteps(testCtx(), []*config.Project{p}, "false")[0].Exec(); err == nil {
		t.Error("false must fail the step")
	}

	ghost := &config.Project{Name: "ghost", ProjectDir: t.TempDir() + "/nope"}
	if steps := execSteps(testCtx(), []*config.Project{ghost}, "true"); !steps[0].Skip {
		t.Error("a missing clone must be a skip")
	}
}

// The notes section names the tag, the range, and the commits in it.
func TestNotesSection(t *testing.T) {
	f := newFixture(t)
	p := f.project(t)

	git(t, f.parent, "checkout", "--quiet", "release-1.0")
	git(t, f.parent, "update-ref", "refs/remotes/origin/develop", "develop")
	git(t, f.parent, "update-ref", "refs/remotes/origin/release-1.0", "release-1.0")
	git(t, f.parent, "tag", "1.0.0")

	section := notesSection(testCtx(), p, "")
	if !strings.Contains(section, "## parent 1.0.0") || !strings.Contains(section, "first tag") {
		t.Errorf("first tag section:\n%s", section)
	}

	if !strings.Contains(section, "freeze deps for release-1.0") {
		t.Errorf("the freeze commit is what 1.0.0 has over develop:\n%s", section)
	}

	writeFile(t, f.parent, "hot.txt", "fix\n")
	commit(t, f.parent, "parent: hotfix for 1.0.1")
	git(t, f.parent, "update-ref", "refs/remotes/origin/release-1.0", "release-1.0")
	git(t, f.parent, "tag", "1.0.1")

	section = notesSection(testCtx(), p, "")
	if !strings.Contains(section, "## parent 1.0.1") || !strings.Contains(section, "changes since 1.0.0") {
		t.Errorf("second tag section:\n%s", section)
	}

	if !strings.Contains(section, "hotfix for 1.0.1") || strings.Contains(section, "freeze deps") {
		t.Errorf("only the new work belongs in the section:\n%s", section)
	}
}

// The notes headings come from the config when it says so.
func TestNotesTemplates(t *testing.T) {
	f := newFixture(t)
	p := f.project(t)

	git(t, f.parent, "checkout", "--quiet", "release-1.0")
	git(t, f.parent, "update-ref", "refs/remotes/origin/develop", "develop")
	git(t, f.parent, "update-ref", "refs/remotes/origin/release-1.0", "release-1.0")
	git(t, f.parent, "tag", "1.0.0")

	rctx := testCtx()
	rctx.Cfg.ProductVersion = "2026.06"
	rctx.Cfg.NotesHeader = "# {product_version} ships"
	rctx.Cfg.NotesSectionTitle = "### {project} — {tag} ({branch})"

	if got := notesHeader(rctx); got != "# 2026.06 ships" {
		t.Errorf("header = %q", got)
	}

	section := notesSection(rctx, p, "")
	if !strings.Contains(section, "### parent — 1.0.0 (release-1.0)") {
		t.Errorf("section heading not templated:\n%s", section)
	}

	// Unset templates keep the built-in headings.
	rctx.Cfg.NotesHeader, rctx.Cfg.NotesSectionTitle = "", ""

	if got := notesHeader(rctx); got != "# Release 2026.06" {
		t.Errorf("default header = %q", got)
	}
}
