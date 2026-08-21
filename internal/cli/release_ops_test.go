package cli

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/grinderz/repo-tools/internal/config"
	"github.com/grinderz/repo-tools/internal/gitx"
	"github.com/grinderz/repo-tools/internal/run"
)

func TestNextReleaseBranch(t *testing.T) {
	f := newFixture(t)
	p := f.project(t)
	r := gitx.Repo{Dir: f.parent}

	git(t, f.parent, "branch", "release-1.9", "release-1.0")
	publishOrigin(t, f.parent, "release-1.0", "release-1.9")

	next, err := nextReleaseBranch(r, p, "", false)
	if err != nil || next != "release-1.10" {
		t.Errorf("minor bump: got %q, err %v", next, err)
	}

	next, err = nextReleaseBranch(r, p, "", true)
	if err != nil || next != "release-2.0" {
		t.Errorf("major bump: got %q, err %v", next, err)
	}

	next, err = nextReleaseBranch(r, p, "4.2", false)
	if err != nil || next != "release-4.2" {
		t.Errorf("explicit version: got %q, err %v", next, err)
	}

	if _, err := nextReleaseBranch(r, p, "4", false); err == nil {
		t.Error("a version that is not X.Y must be refused")
	}
}

// The very first release branch of a project has nothing to bump from.
func TestNextReleaseBranchWithoutHistory(t *testing.T) {
	f := newFixture(t)
	p := f.project(t)
	r := gitx.Repo{Dir: f.parent}

	publishOrigin(t, f.parent, "develop")

	next, err := nextReleaseBranch(r, p, "", false)
	if err != nil || next != "release-1.0" {
		t.Errorf("got %q, err %v", next, err)
	}
}

func TestTagForAndTagExists(t *testing.T) {
	f := newFixture(t)
	r := gitx.Repo{Dir: f.parent}
	ver := gitx.ReleaseVer{Major: 1, Minor: 0}

	git(t, f.parent, "tag", "1.0.0")
	git(t, f.parent, "tag", "1.0.1-rc.0")

	rc, err := tagFor(r, ver, tagRC, "")
	if err != nil || rc != "1.0.1-rc.1" {
		t.Errorf("next rc: got %q, err %v", rc, err)
	}

	final, err := tagFor(r, ver, tagFinal, "")
	if err != nil || final != "1.0.1" {
		t.Errorf("next final: got %q, err %v", final, err)
	}

	override, err := tagFor(r, ver, tagRC, "9.9.9")
	if err != nil || override != "9.9.9" {
		t.Errorf("override: got %q, err %v", override, err)
	}

	if !tagExists(r, "1.0.0") || tagExists(r, "7.7.7") {
		t.Error("tag existence is wrong")
	}
}

// A failed push must not leave the tag behind, or the next run would report it
// as already existing and skip the project.
func TestCreateTagRemovesTheTagWhenPushFails(t *testing.T) {
	f := newFixture(t)
	p := f.project(t)
	r := gitx.Repo{Dir: f.parent}

	publishOrigin(t, f.parent, "release-1.0")

	// origin points at the submodule fixture, which will refuse this tag push
	// because that repository has no such branch to authorise... any failure
	// will do: the point is that the local tag does not survive it.
	git(t, f.parent, "remote", "add", "origin", filepath.Join(t.TempDir(), "missing.git"))

	err := createTag(testCtx(), p, "release-1.0", "1.2.3", "release 1.2.3", "")
	if err == nil {
		t.Fatal("expected the push to fail")
	}

	if tagExists(r, "1.2.3") {
		t.Error("the local tag outlived the failed push")
	}
}

func TestSyncProjectFastForwardsTheDevBranch(t *testing.T) {
	f := newFixture(t)
	p := f.project(t)

	git(t, f.parent, "checkout", "--quiet", "develop")
	git(t, f.parent, "submodule", "update", "--quiet", "--", "sub")

	// origin/develop one commit ahead of the local branch.
	writeFile(t, f.parent, "app.txt", "remote work\n")
	ahead := commit(t, f.parent, "parent: remote work")
	publishOrigin(t, f.parent, "develop")
	git(t, f.parent, "reset", "--hard", "--quiet", "HEAD~1")

	rctx := &run.Ctx{Cfg: &config.Config{Confirm: config.ConfirmNever}, FetchFlag: &offline}
	if err := syncProject(rctx, p, false); err != nil {
		t.Fatalf("fast-forward failed: %v", err)
	}

	if head := git(t, f.parent, "rev-parse", "HEAD"); head != ahead {
		t.Errorf("develop was not fast-forwarded: %s", head)
	}
}

func TestSyncProjectRefusesDirtyTree(t *testing.T) {
	f := newFixture(t)
	p := f.project(t)

	git(t, f.parent, "checkout", "--quiet", "develop")
	git(t, f.parent, "submodule", "update", "--quiet", "--", "sub")
	publishOrigin(t, f.parent, "develop")
	writeFile(t, f.parent, "app.txt", "local edit\n")

	rctx := &run.Ctx{Cfg: &config.Config{Confirm: config.ConfirmNever}, FetchFlag: &offline}

	err := syncProject(rctx, p, false)
	if err == nil || !strings.Contains(err.Error(), "dirty") {
		t.Errorf("got %v, want a dirty-tree refusal", err)
	}
}

// The dev branch is updated even while another branch is checked out.
func TestSyncProjectUpdatesDevBranchFromAnotherBranch(t *testing.T) {
	f := newFixture(t)
	p := f.project(t)

	git(t, f.parent, "checkout", "--quiet", "develop")
	git(t, f.parent, "submodule", "update", "--quiet", "--", "sub")
	writeFile(t, f.parent, "app.txt", "remote work\n")
	ahead := commit(t, f.parent, "parent: remote work")
	publishOrigin(t, f.parent, "develop")
	git(t, f.parent, "reset", "--hard", "--quiet", "HEAD~1")

	git(t, f.parent, "checkout", "--quiet", "release-1.0")
	git(t, f.parent, "submodule", "update", "--quiet", "--", "sub")

	rctx := &run.Ctx{Cfg: &config.Config{Confirm: config.ConfirmNever}, FetchFlag: &offline}
	if err := syncProject(rctx, p, false); err != nil {
		t.Fatalf("sync from another branch failed: %v", err)
	}

	if head := git(t, f.parent, "rev-parse", "develop"); head != ahead {
		t.Errorf("develop was not updated: %s", head)
	}

	if branch := git(t, f.parent, "rev-parse", "--abbrev-ref", "HEAD"); branch != "release-1.0" {
		t.Errorf("the checked out branch changed to %s", branch)
	}
}

func TestPrintCandidatesNumbering(t *testing.T) {
	project := &config.Project{Name: "api", DevBranch: "develop"}
	set := candidateSet{
		Picking: []candidate{
			{Short: "aaa1111", Author: "Ann", Subject: "feat/AB-1: one"},
			{Short: "bbb2222", Author: "Bob", Subject: "fix/AB-2: two"},
		},
		AlreadyInSync: 3,
	}

	numbered := captureOutput(t, func() {
		printCandidates(project, "release-1.0", set, mustFilter(t, []string{"AB-1"}, nil), true)
	})

	if !strings.Contains(numbered, "  1  aaa1111") || !strings.Contains(numbered, "  2  bbb2222") {
		t.Errorf("entries are not numbered: %q", numbered)
	}

	if !strings.Contains(numbered, "selected by [AB-1]") || !strings.Contains(numbered, "(3 already in release-1.0)") {
		t.Errorf("header lacks the filter or the skipped count: %q", numbered)
	}

	plain := captureOutput(t, func() {
		printCandidates(project, "release-1.0", set, mustFilter(t, nil, nil), false)
	})

	if strings.Contains(plain, "  1  aaa1111") {
		t.Errorf("a listing must not be numbered: %q", plain)
	}
}

// A project with release_tags: false is planned as a skip by both tag commands.
func TestPlanTagSkipsProjectsWithTagsDisabled(t *testing.T) {
	f := newFixture(t)
	p := f.project(t)
	off := false
	p.ReleaseTags = &off

	for _, kind := range []tagKind{tagRC, tagFinal} {
		step, _ := planTag(testCtx(), p, kind, "", "")
		if !step.Skip || !strings.Contains(step.Warn, "release tags are disabled") {
			t.Errorf("kind %v: skip=%v warn=%q", kind, step.Skip, step.Warn)
		}

		if step.Exec != nil {
			t.Errorf("kind %v: a skipped step must not carry work", kind)
		}
	}
}

// A tag message can list what the tag contains: everything since the previous
// tag of the same release branch.
func TestTagMessageListsCommitsSinceThePreviousTag(t *testing.T) {
	f := newFixture(t)
	p := f.project(t)

	git(t, f.parent, "checkout", "--quiet", "release-1.0")
	git(t, f.parent, "tag", "-a", "1.0.0", "-m", "first")

	writeFile(t, f.parent, "app.txt", "hotfix one\n")
	commit(t, f.parent, "fix/AB-1: first hotfix")
	writeFile(t, f.parent, "app.txt", "hotfix two\n")
	commit(t, f.parent, "fix/AB-2: second hotfix")
	publishOrigin(t, f.parent, "develop", "release-1.0")

	rctx := testCtx()
	rctx.Cfg.RcTagMessage = "rc {version} with {commit_count} commit(s):\n{commits}"

	step, _ := planTag(rctx, p, tagRC, "", "")
	if step.Skip {
		t.Fatalf("step was skipped: %s", step.Warn)
	}

	// The plan keeps the message to one line.
	if !strings.Contains(step.Plan[0], "more line(s)") {
		t.Errorf("a multi-line message should be summarised: %q", step.Plan[0])
	}

	// Tag it for real and read the message back out of git.
	if err := step.Exec(); err == nil {
		t.Fatal("the fixture has no origin, so the push must fail")
	}

	// The push failed, so the tag was removed again; build the message directly.
	vars := messageVars(rctx, p, "release-1.0")
	vars[varVersion] = "1.0.1-rc.0"

	ver, _ := gitx.ParseReleaseBranch("release-1.0", "release-")
	if err := addCommitVars(vars, rctx.Cfg.RcTagMessage, gitx.Repo{Dir: f.parent}, p, "release-1.0", ver); err != nil {
		t.Fatal(err)
	}

	msg := expand(rctx.Cfg.RcTagMessage, vars)
	if vars[varCommitCount] != "2" {
		t.Errorf("commit_count = %q, want 2", vars[varCommitCount])
	}

	if !strings.Contains(msg, "fix/AB-1: first hotfix") || !strings.Contains(msg, "fix/AB-2: second hotfix") {
		t.Errorf("both commits should be listed:\n%s", msg)
	}

	// Oldest first, and hashes are listed on their own too.
	if strings.Index(msg, "AB-1") > strings.Index(msg, "AB-2") {
		t.Errorf("commits should be oldest first:\n%s", msg)
	}

	if len(strings.Split(vars[varCommitHashes], "\n")) != 2 {
		t.Errorf("commit_hashes = %q", vars[varCommitHashes])
	}
}

// Without a previous tag the list is what the release branch has and the dev
// branch does not — the cherry-picks.
func TestTagMessageWithoutAPreviousTagListsCherryPicks(t *testing.T) {
	f := newFixture(t)
	p := f.project(t)

	git(t, f.parent, "checkout", "--quiet", "release-1.0")
	writeFile(t, f.parent, "app.txt", "picked\n")
	commit(t, f.parent, "fix/AB-9: picked into the release")
	publishOrigin(t, f.parent, "develop", "release-1.0")

	vars := map[string]string{}
	ver, _ := gitx.ParseReleaseBranch("release-1.0", "release-")

	if err := addCommitVars(vars, "{commits}", gitx.Repo{Dir: f.parent}, p, "release-1.0", ver); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(vars[varCommits], "fix/AB-9: picked into the release") {
		t.Errorf("got %q", vars[varCommits])
	}
}

// A template that asks for no commit list must not read the history at all,
// and the placeholders stay empty rather than turning into stray text.
func TestTagMessageWithoutCommitPlaceholders(t *testing.T) {
	f := newFixture(t)
	p := f.project(t)
	ver, _ := gitx.ParseReleaseBranch("release-1.0", "release-")

	vars := map[string]string{}
	if err := addCommitVars(vars, "release {version}", gitx.Repo{Dir: f.parent}, p, "release-1.0", ver); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{varCommits, varCommitHashes, varCommitCount} {
		if vars[name] != "" {
			t.Errorf("%s = %q, want empty", name, vars[name])
		}
	}
}

// The tag message and the commit it lands on are shown before the tag is
// created; a tag cannot be amended once pushed.
func TestCreateTagShowsTheMessageFirst(t *testing.T) {
	f := newFixture(t)
	p := f.project(t)

	publishOrigin(t, f.parent, "release-1.0")
	git(t, f.parent, "remote", "add", "origin", filepath.Join(t.TempDir(), "missing.git"))

	out := captureOutput(t, func() {
		_ = createTag(testCtx(), p, "release-1.0", "1.2.3", "release 1.2.3\n\n2 commit(s):\nabc123 fix/AB-1: one", "")
	})

	if !strings.Contains(out, "parent is about to tag 1.2.3 on origin/release-1.0") {
		t.Errorf("the tag and its target were not announced:\n%s", out)
	}

	if !strings.Contains(out, "abc123 fix/AB-1: one") {
		t.Errorf("the whole message should be shown, commit list included:\n%s", out)
	}

	// With the review off nothing is printed before the tagging attempt.
	off := false
	quiet := testCtx()
	quiet.DiffFlag = &off

	out = captureOutput(t, func() { _ = createTag(quiet, p, "release-1.0", "1.2.4", "release 1.2.4", "") })
	if strings.Contains(out, "about to tag") {
		t.Errorf("--no-diff should keep it quiet:\n%s", out)
	}
}

// The plan names the commit a release branch would be cut from: cutting one
// merge too late is the mistake worth catching before the push.
func TestPlanReleaseBranchShowsTheSourceCommit(t *testing.T) {
	f := newFixture(t)
	p := f.project(t)
	publishOrigin(t, f.parent, "develop", "release-1.0")

	step, _ := planReleaseBranch(testCtx(), p, "", false)
	plan := strings.Join(step.Plan, "\n")

	if !strings.Contains(plan, "create release-1.1 from origin/develop") {
		t.Fatalf("plan = %q (warn %q)", plan, step.Warn)
	}

	if !strings.Contains(plan, "from ") || !strings.Contains(plan, "parent: add submodule") {
		t.Errorf("the source commit should be named:\n%s", plan)
	}

	if !strings.Contains(plan, "commit(s) since release-1.0") {
		t.Errorf("the distance from the previous release should be shown:\n%s", plan)
	}
}

// A final tag on a branch that has moved past its last rc is a release of
// something nobody tested; the plan says so rather than silently tagging.
func TestFinalTagWarnsWhenTheBranchMovedPastTheRc(t *testing.T) {
	f := newFixture(t)
	p := f.project(t)

	git(t, f.parent, "checkout", "--quiet", "release-1.0")
	git(t, f.parent, "tag", "-a", "1.0.0-rc.0", "-m", "rc")
	writeFile(t, f.parent, "app.txt", "late fix\n")
	commit(t, f.parent, "fix/AB-99: landed after the rc")
	publishOrigin(t, f.parent, "develop", "release-1.0")

	step, _ := planTag(testCtx(), p, tagFinal, "", "")
	if !strings.Contains(step.Warn, "moved 1 commit(s) since 1.0.0-rc.0") {
		t.Errorf("warn = %q", step.Warn)
	}

	// A newer rc on the head describes what would be released, so no warning.
	git(t, f.parent, "tag", "-a", "1.0.0-rc.1", "-m", "rc")
	publishOrigin(t, f.parent, "release-1.0")

	if step, _ := planTag(testCtx(), p, tagFinal, "", ""); strings.Contains(step.Warn, "moved") {
		t.Errorf("an up-to-date rc needs no warning: %q", step.Warn)
	}
}

func TestFinalTagWarnsWithoutAnyRc(t *testing.T) {
	f := newFixture(t)
	p := f.project(t)
	publishOrigin(t, f.parent, "develop", "release-1.0")

	step, _ := planTag(testCtx(), p, tagFinal, "", "")
	if !strings.Contains(step.Warn, "no release candidate was tagged for 1.0.0") {
		t.Errorf("warn = %q", step.Warn)
	}
}

// Tagging a release whose submodules still track the dev branch produces a tag
// that is not pinned to anything.
func TestTagWarnsAboutUnfrozenSubmodules(t *testing.T) {
	f := newFixture(t)
	p := f.project(t)
	enabled := true
	p.DepsFreeze = &enabled
	p.Submodules = []config.Submodule{{Path: "sub", FreezeTo: "release-1.0"}}

	git(t, f.parent, "checkout", "--quiet", "develop")
	publishOrigin(t, f.parent, "develop")
	// release-1.0 of the fixture is frozen; develop is not, so tag that.
	git(t, f.parent, "branch", "release-2.0", "develop")
	publishOrigin(t, f.parent, "release-2.0")

	step, _ := planTag(testCtx(), p, tagRC, "release-2.0", "")
	if !strings.Contains(step.Warn, "submodules differ from freeze_to") ||
		!strings.Contains(step.Warn, "sub tracks develop") {
		t.Errorf("warn = %q", step.Warn)
	}

	// The frozen branch draws no warning.
	if step, _ := planTag(testCtx(), p, tagRC, "release-1.0", ""); strings.Contains(
		step.Warn,
		"differ from freeze_to",
	) {
		t.Errorf("release-1.0 is frozen: %q", step.Warn)
	}
}

// A per-release config names branches before they are cut. Tagging one is not
// a git error to relay: there is no commit to tag and no history to list, and a
// message template with {commits} used to surface that as raw git output.
func TestPlanTagSkipsAPendingReleaseBranch(t *testing.T) {
	f := newFixture(t)
	p := f.project(t)
	p.ReleaseBranch = "release-9.9"

	rctx := testCtx()
	rctx.Cfg.RcTagMessage = "{version}\n\n{commits}"

	step, _ := planTag(rctx, p, tagRC, "", "")
	if !step.Skip || !strings.Contains(step.Warn, "origin/release-9.9 does not exist yet") {
		t.Fatalf("skip=%v warn=%q plan=%v", step.Skip, step.Warn, step.Plan)
	}
}

// The range a tag message lists: the previous tag of this line, else the
// previous line, else the cherry-picks a project with no tags at all has.
func TestTagRange(t *testing.T) {
	p := &config.Project{DevBranch: "develop"}
	ver := gitx.ReleaseVer{Major: 1, Minor: 2}

	if got := tagRange([]string{"1.2.0", "1.1.4"}, p, "release-1.2", ver); got != "1.2.0..origin/release-1.2" {
		t.Errorf("within the line it counts from its own tag: %s", got)
	}

	if got := tagRange([]string{"1.1.4", "1.0.0"}, p, "release-1.2", ver); got != "1.1.4..origin/release-1.2" {
		t.Errorf("a fresh branch counts from the previous line: %s", got)
	}

	if got := tagRange(nil, p, "release-1.2", ver); got != "origin/develop..origin/release-1.2" {
		t.Errorf("with no tags at all only the picks are listed: %s", got)
	}
}

// A local branch left by a push that failed — protected branches, a missed
// key touch — is reused when it holds nothing of its own, and refused when
// it does: rt must not force-move somebody's commits.
func TestEnsureLocalBranch(t *testing.T) {
	f := newFixture(t)
	r := gitx.Repo{Dir: f.parent}

	// The fixture has no origin; any fixed ref serves as the source.
	from := "develop"

	// Fresh: created.
	if err := ensureLocalBranch(r, "release-9.9", from); err != nil {
		t.Fatal(err)
	}

	// Existing at the same commit: reused, not an error.
	if err := ensureLocalBranch(r, "release-9.9", from); err != nil {
		t.Fatalf("a clean leftover must be reused: %v", err)
	}

	// With its own commit: refused.
	if _, err := r.Git("checkout", "-q", "release-9.9"); err != nil {
		t.Fatal(err)
	}

	writeFile(t, f.parent, "own.txt", "local work")
	commit(t, f.parent, "local-only commit")

	if _, err := r.Git("checkout", "-q", "develop"); err != nil {
		t.Fatal(err)
	}

	err := ensureLocalBranch(r, "release-9.9", from)
	if err == nil || !strings.Contains(err.Error(), "inspect it first") {
		t.Errorf("got %v", err)
	}
}

// Rerunning a flow over an unchanged branch must not mint a new tag for the
// same commit; the final that promotes an rc is the one exception.
func TestPlanTagSkipsAnAlreadyTaggedHead(t *testing.T) {
	f := newFixture(t)
	p := f.project(t)

	git(t, f.parent, "checkout", "--quiet", "release-1.0")
	git(t, f.parent, "tag", "-a", "1.0.0-rc.0", "-m", "rc")
	publishOrigin(t, f.parent, "develop", "release-1.0")

	step, _ := planTag(testCtx(), p, tagRC, "", "")
	if !step.Skip || !strings.Contains(step.Warn, "1.0.0-rc.0 already tags the head") {
		t.Errorf("rc must skip: skip=%v warn=%q", step.Skip, step.Warn)
	}

	// The final promotes the rc at the same commit, so it is not blocked.
	if step, _ := planTag(testCtx(), p, tagFinal, "", ""); step.Skip {
		t.Errorf("the final must proceed past its own rc: %s", step.Warn)
	}

	git(t, f.parent, "tag", "-a", "1.0.0", "-m", "final")

	if step, _ := planTag(testCtx(), p, tagFinal, "", ""); !step.Skip ||
		!strings.Contains(step.Warn, "1.0.0 already tags the head") {
		t.Errorf("a tagged final must skip: skip=%v warn=%q", step.Skip, step.Warn)
	}

	// New commits move the head, and tagging is work again.
	writeFile(t, f.parent, "app.txt", "hotfix\n")
	commit(t, f.parent, "fix/AB-1: hotfix")
	publishOrigin(t, f.parent, "release-1.0")

	if step, _ := planTag(testCtx(), p, tagRC, "", ""); step.Skip {
		t.Errorf("a moved head must be taggable: %s", step.Warn)
	}
}
