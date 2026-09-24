package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/grinderz/repo-tools/internal/config"
	"github.com/grinderz/repo-tools/internal/gitx"
	"github.com/grinderz/repo-tools/internal/run"
)

// fakeGlab puts a glab on PATH that records its calls and answers the two
// requests the merge request mode makes: the lookup of an open request for a
// branch, which finds one once "mr create" has run, and the creation itself.
// It returns the path of the call log.
func fakeGlab(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	log := filepath.Join(dir, "calls.log")

	script := `#!/bin/sh
echo "$*" >> "$GLAB_LOG"
case "$1 $2" in
  "api projects/:id/merge_requests/"*)
    state=opened; [ -f "$GLAB_LOG.state" ] && state=$(cat "$GLAB_LOG.state")
    echo "{\"state\":\"$state\"}" ;;
  "api projects/:id/merge_requests"*)
    if [ -f "$GLAB_LOG.mr" ]; then echo '[{"web_url":"https://gl/mr/1"}]'; else echo '[]'; fi ;;
  "mr create")
    touch "$GLAB_LOG.mr"; echo "Creating merge request for $4 into $6"; echo "https://gl/mr/1" ;;
  *) echo "unexpected: $*" >&2; exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "glab"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GLAB_LOG", log)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	return log
}

// mrFixture is the submodule fixture with a bare origin for the parent, on
// develop, plus a config with mr: always and ci: gitlab.
func mrFixture(t *testing.T) (fixture, *run.Ctx, *config.Project) {
	t.Helper()

	f := newFixture(t)

	git(t, f.parent, "checkout", "--quiet", "develop")
	git(t, f.parent, "submodule", "update", "--quiet", "--", "sub")

	bare := filepath.Join(t.TempDir(), "parent.git")
	git(t, f.parent, "clone", "--bare", "--quiet", f.parent, bare)
	git(t, f.parent, "remote", "add", "origin", bare)
	git(t, f.parent, "fetch", "--quiet", "origin")
	git(t, f.parent, "branch", "--set-upstream-to=origin/develop", "develop")

	path := filepath.Join(t.TempDir(), "config.yaml")
	body := "projects:\n" +
		"  - name: parent\n" +
		"    git: " + f.sub + "\n" +
		"    ci: gitlab\n" +
		"    mr: always\n" +
		"    project_dir: " + f.parent + "\n"

	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}

	cfg.Confirm = config.ConfirmNever

	// NoCI: the pipeline watch would ask the fake glab questions it cannot
	// answer; the request itself is what is under test.
	rctx := &run.Ctx{Cfg: cfg, FetchFlag: &offline, NoCI: true}

	return f, rctx, cfg.Projects[0]
}

// In merge request mode the commit never touches the target branch: it goes
// to the command's own branch, the request is opened into the target, and
// the working copy is back on the target branch, clean, when it is over.
//
//nolint:paralleltest // puts a fake glab on PATH and captures os.Stdout
func TestCommitAndPushOpensAMergeRequest(t *testing.T) {
	log := fakeGlab(t)
	f, rctx, p := mrFixture(t)
	r := gitx.Repo{Dir: f.parent}

	before := git(t, f.parent, "rev-parse", "origin/develop")
	branch := "rt/deps-submodules/develop/" + time.Now().Format(time.DateOnly)

	writeFile(t, f.parent, "app.txt", "bumped\n")

	var err error

	out := captureOutput(t, func() {
		err = commitAndPush(rctx, r, p, cmdDepsSubmodules, "develop", "chore: bump", "nothing")
	})
	if err != nil {
		t.Fatalf("commitAndPush: %v", err)
	}

	if got := git(t, f.parent, "rev-parse", "origin/develop"); got != before {
		t.Errorf("origin/develop moved from %s to %s; the commit must go to the request branch", before, got)
	}

	pushed := git(t, f.parent, "rev-parse", "origin/"+branch)
	if subject := git(t, f.parent, "log", "-1", "--format=%s", pushed); subject != "chore: bump" {
		t.Errorf("origin/%s is at %q, want the bump commit", branch, subject)
	}

	calls, _ := os.ReadFile(log)
	create := "mr create --source-branch " + branch + " --target-branch develop --title chore: bump"

	if !strings.Contains(string(calls), create) {
		t.Errorf("glab was not asked to open the request:\n%s", calls)
	}

	// With no description in the config the body is the system's business.
	if strings.Contains(string(calls), "--description") {
		t.Errorf("no body was configured, none should be passed:\n%s", calls)
	}

	if !strings.Contains(out, "merge request: https://gl/mr/1") {
		t.Errorf("the request URL is not reported:\n%s", out)
	}

	if !strings.Contains(out, "Commit, push and open MR into develop?") && rctx.Interactive() {
		t.Errorf("the question should cover the request:\n%s", out)
	}

	if cur := git(t, f.parent, "rev-parse", "--abbrev-ref", "HEAD"); cur != "develop" {
		t.Errorf("the working copy was left on %s, want develop", cur)
	}

	if status := git(t, f.parent, "status", "--porcelain"); status != "" {
		t.Errorf("the working copy is not clean afterwards:\n%s", status)
	}

	// A rerun on the same day rewrites the branch with one fresh commit and
	// updates the open request instead of opening a second one.
	writeFile(t, f.parent, "app.txt", "bumped again\n")

	captureOutput(t, func() {
		err = commitAndPush(rctx, r, p, cmdDepsSubmodules, "develop", "chore: bump again", "nothing")
	})

	if err != nil {
		t.Fatalf("second commitAndPush: %v", err)
	}

	if n := git(t, f.parent, "rev-list", "--count", "origin/develop..origin/"+branch); n != "1" {
		t.Errorf("the rerun should leave one commit on the branch, got %s", n)
	}

	calls, _ = os.ReadFile(log)
	if strings.Count(string(calls), "mr create") != 1 {
		t.Errorf("the rerun must reuse the open request:\n%s", calls)
	}
}

// The plan says where the commit goes and what the review will ask; a
// project that cannot open a request is reported by repo check, not at the
// push.
func TestMergeRequestPlanAndCheck(t *testing.T) {
	t.Parallel()

	p := &config.Project{Name: "api", CI: config.CIGitLab, MR: config.MRAlways}
	rctx := &run.Ctx{Cfg: &config.Config{MergeRequest: config.MergeRequest{
		Branch: "rt/{command}/{branch}/{date}", Title: "{message}",
	}}}

	lines := commitPlanLines(rctx, p, cmdChangelogUpdate, "release-1.0", "files changed", "chore: changelog\n\nbody")

	want := "commit to rt/changelog-update/release-1.0/" + time.Now().Format(time.DateOnly) +
		", push, open MR into release-1.0 if files changed"
	if !strings.Contains(lines[0], want) {
		t.Errorf("plan line %q lacks %q", lines[0], want)
	}

	m := planMergeRequest(rctx, p, cmdChangelogUpdate, "release-1.0", "chore: changelog\n\nbody")
	if m.Title != "chore: changelog" {
		t.Errorf("the title should be the commit subject, got %q", m.Title)
	}

	if m.Description != "" || len(m.args("--description")) != 0 {
		t.Errorf("nothing configured, nothing to pass: %+v", m)
	}

	// The body is a template; the people come from the config, the project's
	// own list first.
	rctx.Cfg.MergeRequest.Description = "{command} on {branch} by rt"
	rctx.Cfg.MergeRequest.Assignees = []string{"lead"}
	rctx.Cfg.MergeRequest.Reviewers = []string{"one", "two"}
	p.MRReviewers = []string{"owner"}

	m = planMergeRequest(rctx, p, cmdChangelogUpdate, "release-1.0", "msg")
	got := strings.Join(m.args("--description"), " ")
	want = "--description changelog-update on release-1.0 by rt --assignee lead --reviewer owner"

	if got != want {
		t.Errorf("args = %q, want %q", got, want)
	}

	if !strings.Contains(m.review(), "assigned to lead") || !strings.Contains(m.review(), "reviewed by owner") {
		t.Errorf("the review should name the people:\n%s", m.review())
	}

	if issues := mrCheck(rctx, p); len(issues) != 0 {
		t.Errorf("ci: gitlab can open requests: %v", issues)
	}

	// The wait is in the plan when the config always waits, settle included.
	settle := 30
	rctx.Cfg.MergeRequest.Wait = config.MRWaitAlways
	rctx.Cfg.MergeRequest.SettleSeconds = &settle

	lines = commitPlanLines(rctx, p, cmdChangelogUpdate, "release-1.0", "files changed", "msg")
	if !strings.Contains(lines[0], "then wait for the MR to be merged (wait: always) and 30s more") {
		t.Errorf("plan line %q lacks the wait", lines[0])
	}

	rctx.Cfg.MergeRequest.Wait = config.MRWaitNone
	if got := waitPlan(rctx, p); got != "" {
		t.Errorf("wait: none must plan no wait, got %q", got)
	}

	p.CI = config.CINone
	if issues := mrCheck(rctx, p); len(issues) != 1 || !strings.Contains(issues[0], "ci is none") {
		t.Errorf("ci: none cannot open a request: %v", issues)
	}

	off := false
	rctx.MRFlag = &off

	if m := planMergeRequest(rctx, p, cmdChangelogUpdate, "release-1.0", "msg"); m != nil {
		t.Error("--no-mr must switch the mode off")
	}

	// mr: none is final: --mr does not pull the project in.
	on := true
	rctx.MRFlag = &on
	p.MR = config.MRNone

	if m := planMergeRequest(rctx, p, cmdChangelogUpdate, "release-1.0", "msg"); m != nil {
		t.Error("mr: none must hold under --mr")
	}

	// Unset: only the flag turns it on.
	p.MR = ""
	if m := planMergeRequest(rctx, p, cmdChangelogUpdate, "release-1.0", "msg"); m == nil {
		t.Error("--mr must turn the mode on for a project that leaves mr unset")
	}

	rctx.MRFlag = nil
	if m := planMergeRequest(rctx, p, cmdChangelogUpdate, "release-1.0", "msg"); m != nil {
		t.Error("without --mr an unset mr means no merge request")
	}
}

// The wait is for the projects still to come that pull this one in — as a
// submodule or as a pinned module — and only for those.
func TestMRDependentsLookAheadInTheRun(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	onDevelop(t, f)

	// A go project pinning the library through its make variable.
	gomod := filepath.Join(t.TempDir(), "gomod")
	initRepo(t, gomod)
	writeFile(t, gomod, "Makefile", "GO_DEPS_UPDATE_INTERNAL := "+f.sub+"@develop\n")
	commit(t, gomod, "gomod: pin the library")

	// A project with nothing to do with the library.
	other := filepath.Join(t.TempDir(), "other")
	initRepo(t, other)
	writeFile(t, other, "README", "alone\n")
	commit(t, other, "other: initial")

	path := filepath.Join(t.TempDir(), "config.yaml")
	body := "cmds: {deps: {go: [\"go mod tidy\"]}}\n" +
		"deps_pins:\n  go:\n    file: Makefile\n    var: GO_DEPS_UPDATE_INTERNAL\n" +
		"projects:\n" +
		"  - name: lib\n    git: " + f.sub + "\n    project_dir: " + f.sub + "\n" +
		"  - name: parent\n    git: " + f.parent + "\n    project_dir: " + f.parent + "\n" +
		"  - name: gomod\n    git: " + gomod + "\n    project_dir: " + gomod + "\n    deps: go\n" +
		"  - name: other\n    git: " + other + "\n    project_dir: " + other + "\n"

	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}

	rctx := &run.Ctx{Cfg: cfg, FetchFlag: &offline, Selection: cfg.Projects}
	lib, parent := cfg.Projects[0], cfg.Projects[1]

	if got := mrDependents(rctx, lib); strings.Join(got, ",") != "parent,gomod" {
		t.Errorf("lib's dependents = %v, want parent and gomod", got)
	}

	if got := mrDependents(rctx, parent); len(got) != 0 {
		t.Errorf("nothing depends on parent, got %v", got)
	}

	// Only the projects after this one count: a run over parent alone waits
	// for nobody.
	rctx.Selection = []*config.Project{parent, lib}
	if got := mrDependents(rctx, lib); len(got) != 0 {
		t.Errorf("parent runs before lib and cannot wait for it, got %v", got)
	}

	if got := mrWaitFor(rctx, lib); got != nil {
		t.Errorf("dependents: no wait without dependents, got %v", got)
	}

	cfg.MergeRequest.Wait = config.MRWaitAlways

	if got := mrWaitFor(rctx, lib); len(got) != 1 {
		t.Errorf("always: wait whatever the run, got %v", got)
	}
}

// The wait holds the batch until the request is merged, then settles; a
// request closed without a merge fails the step, since the next project
// would pull a dev branch without the change.
//
//nolint:paralleltest // puts a fake glab on PATH and captures os.Stdout
func TestWaitMergedPollsUntilMerged(t *testing.T) {
	log := fakeGlab(t)
	f, rctx, p := mrFixture(t)
	r := gitx.Repo{Dir: f.parent}

	poll, settle := 1, 1
	rctx.Cfg.CIPollSeconds = &poll
	rctx.Cfg.MergeRequest.SettleSeconds = &settle

	// Merged a moment after the wait starts, so it has to poll more than once.
	go func() {
		time.Sleep(1500 * time.Millisecond)

		_ = os.WriteFile(log+".state", []byte("merged"), 0o644)
	}()

	var err error

	started := time.Now()

	out := captureOutput(t, func() {
		err = waitMerged(rctx, r, p, "https://gl/mr/7", []string{"api"})
	})
	if err != nil {
		t.Fatalf("wait: %v", err)
	}

	if elapsed := time.Since(started); elapsed < 2*time.Second {
		t.Errorf("the wait returned after %s, before the merge plus the settle", elapsed)
	}

	wants := []string{"waiting for https://gl/mr/7 to be merged (needed by api)", "merged", "settling for 1s"}

	for _, want := range wants {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}

	calls, _ := os.ReadFile(log)
	if !strings.Contains(string(calls), "api projects/:id/merge_requests/7") {
		t.Errorf("the request was not asked by its iid:\n%s", calls)
	}

	// Closed without a merge: the change is not coming.
	if err := os.WriteFile(log+".state", []byte("closed"), 0o644); err != nil {
		t.Fatal(err)
	}

	captureOutput(t, func() { err = waitMerged(rctx, r, p, "https://gl/mr/7", []string{"api"}) })

	if err == nil || !strings.Contains(err.Error(), "closed without being merged") {
		t.Errorf("a closed request must fail the wait, got %v", err)
	}
}

// The web form fills the body from the repository's template and the API
// does not, so a template the config names is read from the target branch,
// expanded and sent as the body.
//
//nolint:paralleltest // puts a fake glab on PATH and captures os.Stdout
func TestCommitAndPushFillsTheBodyFromTheTemplate(t *testing.T) {
	log := fakeGlab(t)
	f, rctx, p := mrFixture(t)
	r := gitx.Repo{Dir: f.parent}

	if err := os.MkdirAll(filepath.Join(f.parent, ".gitlab/merge_request_templates"), 0o755); err != nil {
		t.Fatal(err)
	}

	writeFile(t, f.parent, ".gitlab/merge_request_templates/Default.md",
		"#### Description\n{message}\n\n### CR\n- [ ] @lead\n")
	commit(t, f.parent, "parent: add the merge request template")
	git(t, f.parent, "push", "--quiet", "origin", "develop")

	rctx.Cfg.MergeRequest.Template = "Default"

	writeFile(t, f.parent, "app.txt", "bumped\n")

	var err error

	out := captureOutput(t, func() {
		err = commitAndPush(rctx, r, p, cmdDepsSubmodules, "develop", "chore: bump", "nothing")
	})
	if err != nil {
		t.Fatalf("commitAndPush: %v", err)
	}

	calls, _ := os.ReadFile(log)
	if !strings.Contains(string(calls), "--description #### Description\nchore: bump\n\n### CR\n- [ ] @lead") {
		t.Errorf("the expanded template was not sent as the body:\n%s", calls)
	}

	if !strings.Contains(out, "body from .gitlab/merge_request_templates/Default.md") {
		t.Errorf("the review should say where the body comes from:\n%s", out)
	}
}

// A template the config names but the branch lacks is a check finding and an
// error at the request, not a silently empty body.
func TestMissingMergeRequestTemplateIsReported(t *testing.T) {
	t.Parallel()

	p := newFixture(t).project(t) // a real clone, without a template
	p.CI, p.MR = config.CIGitLab, config.MRAlways

	rctx := ctxFor(p)
	rctx.Cfg.MergeRequest = config.MergeRequest{
		Branch: "rt/{command}/{branch}/{date}", Title: "{message}", Template: "Default",
	}

	if issues := mrCheck(rctx, p); len(issues) != 1 || !strings.Contains(issues[0], "Default.md is not on develop") {
		t.Errorf("a missing template should be reported: %v", issues)
	}

	m := planMergeRequest(rctx, p, cmdChangelogUpdate, "develop", "msg")
	if m.BodyErr == nil || m.Description != "" {
		t.Errorf("a missing template must not pass as an empty body: %+v", m)
	}

	if _, _, err := commitToMergeRequest(rctx, gitx.Repo{Dir: p.Dir()}, p, m, "msg"); err == nil ||
		!strings.Contains(err.Error(), "template not found") {
		t.Errorf("the request must refuse to open with the template missing, got %v", err)
	}
}
