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

// online is the --fetch override for the submodule tests: the submodule's
// origin is a local path, so fetching it costs nothing and is what makes a new
// upstream commit visible.
var online = true //nolint:gochecknoglobals // shared by the submodule tests

func fetchingCtx() *run.Ctx {
	return &run.Ctx{Cfg: &config.Config{Confirm: config.ConfirmNever}, FetchFlag: &online}
}

// onDevelop puts the fixture parent on develop with its submodule checked out,
// which is the state deps submodules runs in.
func onDevelop(t *testing.T, f fixture) {
	t.Helper()

	git(t, f.parent, "checkout", "--quiet", "develop")
	git(t, f.parent, "submodule", "update", "--quiet", "--", "sub")
}

func TestTrackedSubmodulesReadsTheBranchFromGitmodules(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	onDevelop(t, f)

	r := gitx.Repo{Dir: f.parent}

	targets, untracked, err := trackedSubmodules(r, "", nil)
	if err != nil {
		t.Fatal(err)
	}

	if len(untracked) != 0 {
		t.Errorf("nothing should be untracked here: %v", untracked)
	}

	if len(targets) != 1 || targets[0].Path != "sub" || targets[0].Branch != "develop" {
		t.Fatalf("targets = %+v, want sub tracking develop", targets)
	}
}

// A submodule with no branch in .gitmodules must be reported, not moved:
// --remote would otherwise follow whatever the remote's default branch is.
func TestTrackedSubmodulesSeparatesBranchlessOnes(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	onDevelop(t, f)
	git(t, f.parent, "config", "-f", ".gitmodules", "--unset", "submodule.sub.branch")

	targets, untracked, err := trackedSubmodules(gitx.Repo{Dir: f.parent}, "", nil)
	if err != nil {
		t.Fatal(err)
	}

	if len(targets) != 0 {
		t.Errorf("a branchless submodule must not be a target: %+v", targets)
	}

	if len(untracked) != 1 || untracked[0] != "sub" {
		t.Errorf("untracked = %v, want [sub]", untracked)
	}
}

func TestTrackedSubmodulesFilter(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	onDevelop(t, f)

	r := gitx.Repo{Dir: f.parent}

	targets, _, err := trackedSubmodules(r, "", []string{"sub"})
	if err != nil {
		t.Fatal(err)
	}

	if len(targets) != 1 {
		t.Fatalf("the named submodule should be kept: %+v", targets)
	}

	targets, _, err = trackedSubmodules(r, "", []string{"other"})
	if err != nil {
		t.Fatal(err)
	}

	if len(targets) != 0 {
		t.Fatalf("an unnamed submodule should be dropped: %+v", targets)
	}
}

// The pin must end up on the head of the tracked branch, including commits
// pushed after the submodule was cloned — which is why --remote is preceded by
// a fetch inside the submodule.
//
//nolint:paralleltest // captures os.Stdout, which is process-wide
func TestUpdateSubmodulesMovesThePinToTheBranchHead(t *testing.T) {
	f := newFixture(t)
	onDevelop(t, f)

	writeFile(t, f.sub, "lib.txt", "v3\n")
	newHead := commit(t, f.sub, "sub: newer develop work")

	before := subPin(t, f.parent)
	if before == newHead {
		t.Fatal("the fixture already pointed at the new head")
	}

	targets := []submoduleTarget{{Path: "sub", Branch: "develop"}}

	opts := submodulesOpts{NoCommit: true}

	out := captureOutput(t, func() {
		if err := updateSubmodules(fetchingCtx(), f.project(t), "develop", targets, opts); err != nil {
			t.Errorf("update failed: %v", err)
		}
	})

	if got := subPin(t, f.parent); got != newHead {
		t.Errorf("pin = %s, want the develop head %s", got, newHead)
	}

	if !strings.Contains(out, "left uncommitted") {
		t.Errorf("--no-commit should say what it left behind:\n%s", out)
	}

	if status := git(t, f.parent, "status", "--porcelain"); status == "" {
		t.Error("--no-commit must leave the new pin in the working tree")
	}
}

// Nothing to move is a normal outcome, and with --no-commit it must not be
// reported as a change.
//
//nolint:paralleltest // captures os.Stdout, which is process-wide
func TestUpdateSubmodulesIsANoopWhenAlreadyAtTheHead(t *testing.T) {
	f := newFixture(t)
	onDevelop(t, f)

	targets := []submoduleTarget{{Path: "sub", Branch: "develop"}}

	opts := submodulesOpts{NoCommit: true}

	out := captureOutput(t, func() {
		if err := updateSubmodules(fetchingCtx(), f.project(t), "develop", targets, opts); err != nil {
			t.Errorf("update failed: %v", err)
		}
	})

	if !strings.Contains(out, "already at the head") {
		t.Errorf("an unchanged pin should say so:\n%s", out)
	}

	if status := git(t, f.parent, "status", "--porcelain"); status != "" {
		t.Errorf("nothing should have changed:\n%s", status)
	}
}

// The commit path leaves the moved pin in a commit on the requested branch.
//
//nolint:paralleltest // captures os.Stdout, which is process-wide
func TestUpdateSubmodulesCommitsTheNewPin(t *testing.T) {
	f := newFixture(t)
	onDevelop(t, f)

	writeFile(t, f.sub, "lib.txt", "v3\n")
	newHead := commit(t, f.sub, "sub: newer develop work")

	rctx := fetchingCtx()
	rctx.Cfg.SubmodulesCommitMessage = "chore(deps): update submodules on {branch}"

	targets := []submoduleTarget{{Path: "sub", Branch: "develop"}}

	captureOutput(t, func() {
		// No origin in the fixture, so the push at the end fails; the commit
		// before it is what this test is about.
		_ = updateSubmodules(rctx, f.project(t), "develop", targets, submodulesOpts{})
	})

	if got := subPin(t, f.parent); got != newHead {
		t.Errorf("pin = %s, want %s", got, newHead)
	}

	if msg := git(t, f.parent, "log", "-1", "--format=%s"); msg != "chore(deps): update submodules on develop" {
		t.Errorf("commit subject = %q", msg)
	}

	if status := git(t, f.parent, "status", "--porcelain"); status != "" {
		t.Errorf("the pin should be committed, not left in the tree:\n%s", status)
	}
}

func TestNoSubmodulesReason(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		only      []string
		untracked []string
		want      string
	}{
		{"no submodules at all", nil, nil, "project has no submodules"},
		{"none tracks a branch", nil, []string{"sub"}, "no submodule tracks a branch"},
		{"filtered, none exists", []string{"nope"}, nil, "none of the submodules"},
		{"filtered, branchless", []string{"sub"}, []string{"sub"}, "have no branch in .gitmodules"},
	}

	for _, tc := range cases {
		if got := noSubmodulesReason(tc.only, tc.untracked); !strings.Contains(got, tc.want) {
			t.Errorf("%s: got %q, want it to mention %q", tc.name, got, tc.want)
		}
	}
}

// A project whose working copy is missing is planned as a skip, never as work.
func TestPlanSubmodulesSkipsMissingRepo(t *testing.T) {
	t.Parallel()

	p := &config.Project{Name: "ghost", DevBranch: "develop"}

	step, matched, planErr := planSubmodules(fetchingCtx(), p, submodulesOpts{})
	if planErr != nil {
		t.Fatal(planErr)
	}

	if !step.Skip || matched != 0 {
		t.Fatalf("skip = %v, matched = %d", step.Skip, matched)
	}

	if !strings.Contains(step.Warn, "not cloned") {
		t.Errorf("warn = %q", step.Warn)
	}
}

func TestPlanSubmodulesUsesTheDevBranchAndReportsTargets(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	onDevelop(t, f)

	step, matched, _ := planSubmodules(testCtx(), f.project(t), submodulesOpts{})
	if step.Skip {
		t.Fatalf("step was skipped: %s", step.Warn)
	}

	if matched != 1 {
		t.Errorf("matched = %d, want 1", matched)
	}

	plan := strings.Join(step.Plan, "\n")
	if !strings.Contains(plan, "checkout develop") {
		t.Errorf("the dev branch should be the default:\n%s", plan)
	}

	if !strings.Contains(plan, "submodule sub -> head of develop") {
		t.Errorf("the plan should name the target branch:\n%s", plan)
	}

	if !strings.Contains(plan, "commit+push to origin/develop") {
		t.Errorf("the plan should end in a commit:\n%s", plan)
	}

	step, _, _ = planSubmodules(testCtx(), f.project(t), submodulesOpts{Branch: "release-1.0", NoCommit: true})
	plan = strings.Join(step.Plan, "\n")

	if !strings.Contains(plan, "checkout release-1.0") {
		t.Errorf("--branch should win over the dev branch:\n%s", plan)
	}

	if !strings.Contains(plan, "leave the new pins uncommitted") {
		t.Errorf("--no-commit should be visible in the plan:\n%s", plan)
	}
}

// The deps commands come from the config and run after the pins move, so a
// lock file regenerated from a submodule lands in the same commit.
//
//nolint:paralleltest // captures os.Stdout, which is process-wide
func TestUpdateSubmodulesRunsTheConfiguredDepsCmds(t *testing.T) {
	f := newFixture(t)
	onDevelop(t, f)

	p := f.project(t)
	p.Deps = "uv"

	rctx := fetchingCtx()
	rctx.Cfg.DepsCmds = map[string][]string{"uv": {"echo {project} > deps.txt"}}

	targets := []submoduleTarget{{Path: "sub", Branch: "develop"}}

	captureOutput(t, func() {
		if err := updateSubmodules(rctx, p, "develop", targets, submodulesOpts{NoCommit: true}); err != nil {
			t.Errorf("update failed: %v", err)
		}
	})

	body, err := os.ReadFile(filepath.Join(f.parent, "deps.txt"))
	if err != nil {
		t.Fatalf("the deps command did not run: %v", err)
	}

	if strings.TrimSpace(string(body)) != "parent" {
		t.Errorf("deps.txt = %q, want the expanded {project}", body)
	}

	// --no-deps must leave it alone.
	if err := os.Remove(filepath.Join(f.parent, "deps.txt")); err != nil {
		t.Fatal(err)
	}

	captureOutput(t, func() {
		if err := updateSubmodules(rctx, p, "develop", targets, submodulesOpts{NoCommit: true, NoDeps: true}); err != nil {
			t.Errorf("update failed: %v", err)
		}
	})

	if _, err := os.Stat(filepath.Join(f.parent, "deps.txt")); err == nil {
		t.Error("--no-deps still ran the deps commands")
	}
}

// The plan names the deps commands, so nothing runs unannounced.
func TestPlanSubmodulesShowsTheDepsCmds(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	onDevelop(t, f)

	p := f.project(t)
	p.Deps = "uv"

	rctx := testCtx()
	rctx.Cfg.DepsCmds = map[string][]string{"uv": {"uv sync"}}

	step, _, _ := planSubmodules(rctx, p, submodulesOpts{})
	if !strings.Contains(strings.Join(step.Plan, "\n"), "deps (uv): uv sync") {
		t.Errorf("plan = %v", step.Plan)
	}

	step, _, _ = planSubmodules(rctx, p, submodulesOpts{NoDeps: true})
	if strings.Contains(strings.Join(step.Plan, "\n"), "deps (") {
		t.Errorf("--no-deps should not plan deps commands: %v", step.Plan)
	}
}

// The declarations must come from the branch the run targets, not from the one
// that happens to be checked out. Reading the working tree instead would take
// a release branch's frozen pin and move it back onto the dev branch.
func TestPlanSubmodulesReadsGitmodulesOfTheTargetBranch(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	onDevelop(t, f)
	publishOrigin(t, f.parent, "develop", "release-1.0")

	// develop tracks develop, release-1.0 was frozen onto release-1.0.
	step, _, _ := planSubmodules(testCtx(), f.project(t), submodulesOpts{Branch: "release-1.0"})
	if step.Skip {
		t.Fatalf("step was skipped: %s", step.Warn)
	}

	plan := strings.Join(step.Plan, "\n")
	if !strings.Contains(plan, "submodule sub -> head of release-1.0") {
		t.Errorf("the release branch pins sub to release-1.0:\n%s", plan)
	}

	if strings.Contains(plan, "head of develop") {
		t.Errorf("the checked-out branch must not decide the target:\n%s", plan)
	}
}

// A submodule that exists on the checked-out branch but not on the target one
// is not part of the run at all.
func TestTrackedSubmodulesAtARefWithoutTheSubmodule(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	onDevelop(t, f)

	// The first commit of the fixture predates the submodule.
	root := git(t, f.parent, "rev-list", "--max-parents=0", "HEAD")

	targets, untracked, err := trackedSubmodules(gitx.Repo{Dir: f.parent}, root, nil)
	if err != nil {
		t.Fatal(err)
	}

	if len(targets) != 0 || len(untracked) != 0 {
		t.Errorf("a ref without .gitmodules has nothing to move: %+v %v", targets, untracked)
	}
}

// A run that dies on the second submodule has already moved the first one; the
// error says what is left behind, or the next run's "working tree is dirty"
// comes out of nowhere.
//
//nolint:paralleltest // captures os.Stdout, which is process-wide
func TestUpdateSubmodulesReportsWhatAFailureLeftBehind(t *testing.T) {
	f := newFixture(t)
	onDevelop(t, f)

	writeFile(t, f.sub, "lib.txt", "v3\n")
	commit(t, f.sub, "sub: newer develop work")

	targets := []submoduleTarget{
		{Path: "sub", Branch: "develop"},
		{Path: "sub", Branch: "no-such-branch"}, // the second one fails
	}

	var err error

	out := captureOutput(t, func() {
		err = updateSubmodules(fetchingCtx(), f.project(t), "develop", targets, submodulesOpts{})
	})

	if err == nil {
		t.Fatal("expected the second submodule to fail the run")
	}

	if !strings.Contains(out, "stopped with changes left in") {
		t.Errorf("the leftover pin was not reported:\n%s", out)
	}

	if !strings.Contains(out, "discard") {
		t.Errorf("no way out was offered:\n%s", out)
	}

	if status := git(t, f.parent, "status", "--porcelain"); status == "" {
		t.Error("the fixture should be dirty, which is the point of the message")
	}
}

// --allow-dirty is for finishing what a --no-commit run started: it begins on
// a dirty tree and says what it found, but only where the diff review can
// still be answered.
//
//nolint:paralleltest // captures os.Stdout, which is process-wide
func TestAllowDirty(t *testing.T) {
	f := newFixture(t)
	onDevelop(t, f)

	writeFile(t, f.parent, "app.txt", "work from an earlier run\n")

	r := gitx.Repo{Dir: f.parent}
	rctx := fetchingCtx()

	if err := startDirty(rctx, r, false); err == nil {
		t.Fatal("without the flag a dirty tree is refused")
	}

	// confirm: never means nobody is there to answer, so the flag is refused.
	if err := startDirty(rctx, r, true); err == nil ||
		!strings.Contains(err.Error(), "someone to answer") {
		t.Fatalf("err = %v", err)
	}

	rctx.Cfg.Confirm = config.ConfirmDestructive

	out := captureOutput(t, func() {
		if err := startDirty(rctx, r, true); err != nil {
			t.Errorf("with the review on it should proceed: %v", err)
		}
	})

	if !strings.Contains(out, "starting on a dirty tree") || !strings.Contains(out, "app.txt") {
		t.Errorf("what was already there must be named:\n%s", out)
	}

	// A clean tree needs no announcement.
	git(t, f.parent, "checkout", "--", "app.txt")

	if out := captureOutput(t, func() { _ = startDirty(rctx, r, true) }); out != "" {
		t.Errorf("nothing to say about a clean tree: %q", out)
	}
}

// A submodule moved by --remote may itself record submodule pins; they have
// to follow to the recorded pins, or the parent is left with "modified
// content" — dirt inside the submodule that no commit of the parent can
// absorb. A service -> its library -> the tooling submodule inside failed
// exactly this way on a real release.
func TestUpdateSubmoduleRemoteFollowsNestedPins(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	inner := filepath.Join(root, "inner")
	mid := filepath.Join(root, "mid")
	parent := filepath.Join(root, "parent")

	initRepo(t, inner)
	writeFile(t, inner, "lib.txt", "v1\n")
	commit(t, inner, "inner: v1")

	initRepo(t, mid)
	writeFile(t, mid, "app.txt", "v1\n")
	commit(t, mid, "mid: v1")
	git(t, mid, "submodule", "add", "--quiet", inner, "inner")
	commit(t, mid, "mid: add inner")

	initRepo(t, parent)
	writeFile(t, parent, "top.txt", "v1\n")
	commit(t, parent, "parent: v1")
	git(t, parent, "submodule", "add", "--quiet", "-b", "develop", mid, "mid")
	git(t, parent, "-c", "protocol.file.allow=always",
		"submodule", "update", "--init", "--recursive")
	commit(t, parent, "parent: add mid")

	// mid moves its inner pin forward; the parent does not know yet.
	writeFile(t, inner, "lib.txt", "v2\n")
	commit(t, inner, "inner: v2")
	git(t, filepath.Join(mid, "inner"), "pull", "--quiet", "origin", "develop")
	commit(t, mid, "mid: bump inner")

	p := &config.Project{Name: "parent", DevBranch: "develop"}
	r := gitx.Repo{Dir: parent}

	if err := updateSubmoduleRemote(testCtx(), p, r, "mid", "develop"); err != nil {
		t.Fatal(err)
	}

	// The gitlink moved — that is the change the freeze commits — but inside
	// the submodule everything must sit on the recorded pins.
	if out := git(t, filepath.Join(parent, "mid"), "status", "--porcelain"); out != "" {
		t.Errorf("mid must be clean inside:\n%s", out)
	}
}
