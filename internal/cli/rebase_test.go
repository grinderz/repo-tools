package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grinderz/repo-tools/internal/gitx"
)

// subCommit adds a commit on the given branch of the fixture's submodule
// origin and returns its sha.
func subCommit(t *testing.T, f fixture, branch, file, body string) string {
	t.Helper()
	git(t, f.sub, "checkout", "--quiet", branch)
	writeFile(t, f.sub, file, body)

	return commit(t, f.sub, "sub: "+file)
}

// bumpPin moves the parent's submodule pin to the given commit and stages it.
func bumpPin(t *testing.T, f fixture, sha string) {
	t.Helper()
	git(t, filepath.Join(f.parent, "sub"), "fetch", "--quiet", "origin")
	git(t, filepath.Join(f.parent, "sub"), "checkout", "--quiet", sha)
	git(t, f.parent, "add", "sub")
}

// Cross-repo feature work: the parent's feature branch pins the submodule's
// own feature branch, both get rebased, and the conflicting pin resolves to
// the submodule feature branch's fresh (rebased) head — the pin recorded
// before that rebase no longer exists anywhere on it.
func TestRebaseResolvesSubmodulePin(t *testing.T) {
	f := newFixture(t)
	p := f.project(t)
	r := gitx.Repo{Dir: f.parent}

	git(t, f.parent, "checkout", "--quiet", "develop")
	git(t, f.parent, "branch", "feature")

	// The submodule grows a feature branch; the parent's feature pins it.
	git(t, f.sub, "branch", "feature", "develop")
	shaA := subCommit(t, f, "feature", "feature.txt", "feature work\n")

	git(t, f.parent, "checkout", "--quiet", "feature")
	bumpPin(t, f, shaA)
	writeFile(t, f.parent, "feature.txt", "work\n")
	commit(t, f.parent, "parent: feature work plus pin bump")

	// Both develops move on; the submodule feature branch is rebased onto
	// its develop, so the old pin shaA is rewritten out of existence.
	shaB := subCommit(t, f, "develop", "dev.txt", "v4\n")

	git(t, f.parent, "checkout", "--quiet", "develop")
	bumpPin(t, f, shaB)
	commit(t, f.parent, "parent: develop pin bump")

	git(t, f.sub, "checkout", "--quiet", "feature")
	git(t, f.sub, "rebase", "--quiet", "develop")
	rebasedHead := git(t, f.sub, "rev-parse", "feature")
	git(t, f.sub, "checkout", "--quiet", "develop")

	if err := rebaseProject(testCtx(), r, p, "feature", "develop", rebaseOpts{NoPush: true}); err != nil {
		t.Fatalf("rebase should have resolved the pin: %v", err)
	}

	if rebaseInProgress(r) {
		t.Fatal("rebase left in progress")
	}

	if got := subPin(t, f.parent); got != rebasedHead {
		t.Errorf("pin = %s, want the rebased submodule feature head %s", got, rebasedHead)
	}

	if _, err := os.Stat(filepath.Join(f.parent, "feature.txt")); err != nil {
		t.Error("the feature's own change was lost")
	}

	out := git(t, f.parent, "log", "--oneline", "develop..feature")
	if strings.Count(out, "\n")+1 != 1 {
		t.Errorf("feature should carry exactly its one commit:\n%s", out)
	}
}

// Without a matching submodule branch the pin falls back to the tracked
// branch — allowed only once that branch has absorbed the feature's pin,
// here through a merge.
func TestRebaseFallsBackToTheTrackedBranch(t *testing.T) {
	f := newFixture(t)
	p := f.project(t)
	r := gitx.Repo{Dir: f.parent}

	git(t, f.parent, "checkout", "--quiet", "develop")
	git(t, f.parent, "branch", "feature")

	// Diverged pins: the feature pins side work, develop pins its own commit.
	git(t, f.sub, "checkout", "--quiet", "-b", "side", "develop")
	writeFile(t, f.sub, "side.txt", "side work\n")
	sideSha := commit(t, f.sub, "sub: side work")

	git(t, f.parent, "checkout", "--quiet", "feature")
	bumpPin(t, f, sideSha)
	writeFile(t, f.parent, "feature.txt", "work\n")
	commit(t, f.parent, "parent: pin the side work")

	shaB := subCommit(t, f, "develop", "dev.txt", "v4\n")

	git(t, f.parent, "checkout", "--quiet", "develop")
	bumpPin(t, f, shaB)
	commit(t, f.parent, "parent: develop pin bump")

	// The side work lands on the tracked branch, so resolving drops nothing.
	git(t, f.sub, "merge", "--quiet", "--no-edit", "side")
	mergedHead := git(t, f.sub, "rev-parse", "develop")

	if err := rebaseProject(testCtx(), r, p, "feature", "develop", rebaseOpts{NoPush: true}); err != nil {
		t.Fatalf("rebase should have resolved the pin: %v", err)
	}

	if got := subPin(t, f.parent); got != mergedHead {
		t.Errorf("pin = %s, want the merged tracked head %s", got, mergedHead)
	}
}

// A conflict outside a submodule is a human's job: the rebase is aborted and
// the branch stays where it was.
func TestRebaseAbortsOnRegularConflict(t *testing.T) {
	f := newFixture(t)
	p := f.project(t)
	r := gitx.Repo{Dir: f.parent}

	git(t, f.parent, "checkout", "--quiet", "develop")
	git(t, f.parent, "branch", "feature")

	git(t, f.parent, "checkout", "--quiet", "feature")
	writeFile(t, f.parent, "app.txt", "from feature\n")
	before := commit(t, f.parent, "parent: feature edit")

	git(t, f.parent, "checkout", "--quiet", "develop")
	writeFile(t, f.parent, "app.txt", "from develop\n")
	commit(t, f.parent, "parent: develop edit")

	err := rebaseProject(testCtx(), r, p, "feature", "develop", rebaseOpts{NoPush: true})
	if err == nil || !strings.Contains(err.Error(), "conflict in non-submodule path") {
		t.Fatalf("err = %v", err)
	}

	if rebaseInProgress(r) {
		t.Fatal("the rebase must be aborted, not left in progress")
	}

	if got := git(t, f.parent, "rev-parse", "feature"); got != before {
		t.Errorf("feature moved from %s to %s despite the abort", before, got)
	}
}

// A pin the tracked branch has not absorbed stops the run: resolving over it
// would drop the commit from the superproject.
func TestRebaseRefusesUnmergedPin(t *testing.T) {
	f := newFixture(t)
	p := f.project(t)
	r := gitx.Repo{Dir: f.parent}

	git(t, f.parent, "checkout", "--quiet", "develop")
	git(t, f.parent, "branch", "feature")

	// The feature pins a submodule commit that lives only on a side branch.
	git(t, f.sub, "checkout", "--quiet", "-b", "side", "develop")
	writeFile(t, f.sub, "side.txt", "side work\n")
	sideSha := commit(t, f.sub, "sub: side work")
	git(t, f.sub, "checkout", "--quiet", "develop")

	git(t, f.parent, "checkout", "--quiet", "feature")
	bumpPin(t, f, sideSha)
	writeFile(t, f.parent, "feature.txt", "work\n")
	before := commit(t, f.parent, "parent: pin the side branch")

	shaB := subCommit(t, f, "develop", "dev.txt", "v4\n")

	git(t, f.parent, "checkout", "--quiet", "develop")
	bumpPin(t, f, shaB)
	commit(t, f.parent, "parent: develop pin bump")

	err := rebaseProject(testCtx(), r, p, "feature", "develop", rebaseOpts{NoPush: true})
	if err == nil || !strings.Contains(err.Error(), "rebase the submodule branch first") {
		t.Fatalf("err = %v", err)
	}

	if rebaseInProgress(r) {
		t.Fatal("the rebase must be aborted")
	}

	if got := git(t, f.parent, "rev-parse", "feature"); got != before {
		t.Error("feature must stay where it was")
	}
}

// --submodules does the whole cross-repo dance itself: the submodule's
// feature branch is rebased onto its develop and pushed, and the parent's
// conflicting pin lands on that fresh head. Nothing was rebased by hand.
func TestRebaseSubmodulesRebasesAndPushesTheSubBranch(t *testing.T) {
	f := newFixture(t)
	p := f.project(t)
	r := gitx.Repo{Dir: f.parent}

	git(t, f.parent, "checkout", "--quiet", "develop")
	git(t, f.parent, "branch", "feature")

	git(t, f.sub, "branch", "feature", "develop")
	shaA := subCommit(t, f, "feature", "feature.txt", "feature work\n")

	git(t, f.parent, "checkout", "--quiet", "feature")
	bumpPin(t, f, shaA)
	writeFile(t, f.parent, "feature.txt", "work\n")
	commit(t, f.parent, "parent: feature work plus pin bump")

	shaB := subCommit(t, f, "develop", "dev.txt", "v4\n")

	git(t, f.parent, "checkout", "--quiet", "develop")
	bumpPin(t, f, shaB)
	commit(t, f.parent, "parent: develop pin bump")

	opts := rebaseOpts{NoPush: true, Submodules: true}
	if err := rebaseProject(testCtx(), r, p, "feature", "develop", opts); err != nil {
		t.Fatalf("rebase --submodules failed: %v", err)
	}

	// The submodule's origin got the rebased branch: on top of develop now.
	subFeature := git(t, f.sub, "rev-parse", "feature")
	git(t, f.sub, "merge-base", "--is-ancestor", shaB, subFeature)

	if subFeature == shaA {
		t.Error("the submodule feature branch was not rebased")
	}

	if got := subPin(t, f.parent); got != subFeature {
		t.Errorf("pin = %s, want the pushed submodule head %s", got, subFeature)
	}

	out := git(t, f.parent, "log", "--oneline", "develop..feature")
	if strings.Count(out, "\n")+1 != 1 {
		t.Errorf("the conflict already refreshed the pin, no re-pin commit expected:\n%s", out)
	}
}

// When the parent rebase has no pin conflict to refresh through, the freshly
// pushed submodule head is pinned by an extra commit — otherwise the branch
// would keep pointing at commits the force-push just orphaned.
func TestRebaseSubmodulesRepinsWithoutAConflict(t *testing.T) {
	f := newFixture(t)
	p := f.project(t)
	r := gitx.Repo{Dir: f.parent}

	git(t, f.parent, "checkout", "--quiet", "develop")
	git(t, f.parent, "branch", "feature")

	git(t, f.sub, "branch", "feature", "develop")
	shaA := subCommit(t, f, "feature", "feature.txt", "feature work\n")
	subCommit(t, f, "develop", "dev.txt", "v4\n")

	git(t, f.parent, "checkout", "--quiet", "feature")
	bumpPin(t, f, shaA)
	commit(t, f.parent, "parent: pin the submodule feature")

	// develop moves without touching the pin: the parent rebase will not
	// conflict, so only the re-pin step can pick the new head up.
	git(t, f.parent, "checkout", "--quiet", "develop")
	writeFile(t, f.parent, "app.txt", "moved on\n")
	commit(t, f.parent, "parent: unrelated develop work")

	opts := rebaseOpts{NoPush: true, Submodules: true}
	if err := rebaseProject(testCtx(), r, p, "feature", "develop", opts); err != nil {
		t.Fatalf("rebase --submodules failed: %v", err)
	}

	subFeature := git(t, f.sub, "rev-parse", "feature")
	if subFeature == shaA {
		t.Error("the submodule feature branch was not rebased")
	}

	if got := subPin(t, f.parent); got != subFeature {
		t.Errorf("pin = %s, want the rebased head %s", got, subFeature)
	}

	out := git(t, f.parent, "log", "--format=%s", "develop..feature")
	if !strings.Contains(out, "pin rebased submodule branches") {
		t.Errorf("the re-pin commit is missing:\n%s", out)
	}
}

// A submodule branch named like the one being rebased wins over the tracked
// branch: that is cross-repo feature work.
func TestRebasePinBranchPrefersTheSourceBranch(t *testing.T) {
	f := newFixture(t)
	sub := gitx.Repo{Dir: filepath.Join(f.parent, "sub")}

	git(t, f.parent, "submodule", "update", "--init", "--quiet", "--", "sub")

	if got := rebasePinBranch(sub, "feature", "develop"); got != "develop" {
		t.Errorf("no feature branch in the submodule: got %q", got)
	}

	git(t, f.sub, "branch", "feature", "develop")

	if got := rebasePinBranch(sub, "feature", "develop"); got != "feature" {
		t.Errorf("the submodule's own feature branch must win: got %q", got)
	}
}
