package cli

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grinderz/repo-tools/internal/config"
	"github.com/grinderz/repo-tools/internal/gitx"
	"github.com/grinderz/repo-tools/internal/run"
)

// captureOutput collects what fn prints to stdout.
func captureOutput(t *testing.T, fn func()) string {
	t.Helper()

	stdout := os.Stdout

	pipeR, pipeW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}

	os.Stdout = pipeW

	done := make(chan string)

	go func() {
		var collected strings.Builder

		buf := make([]byte, 4096)

		for {
			n, err := pipeR.Read(buf)
			collected.Write(buf[:n])

			if err != nil {
				break
			}
		}

		done <- collected.String()
	}()

	fn()

	if err := pipeW.Close(); err != nil {
		t.Fatal(err)
	}

	os.Stdout = stdout

	return <-done
}

func TestExpand(t *testing.T) {
	t.Parallel()

	vars := map[string]string{varVersion: "1.2.3", varBranch: "release-1.2", varProject: "api"}

	got := expand("tag {version} on {branch} of {project}", vars)
	if got != "tag 1.2.3 on release-1.2 of api" {
		t.Errorf("got %q", got)
	}

	// An unknown placeholder is left alone rather than blanked out.
	if got := expand("{nope} {version}", vars); got != "{nope} 1.2.3" {
		t.Errorf("got %q", got)
	}
}

func TestFirstLineAndShorten(t *testing.T) {
	t.Parallel()

	if got := firstLine("one\ntwo\nthree"); got != "one" {
		t.Errorf("firstLine = %q", got)
	}

	if got := firstLine("  only  "); got != "only" {
		t.Errorf("firstLine should trim: %q", got)
	}

	if got := shorten("0123456789abcdef", 8); got != "01234567" {
		t.Errorf("shorten = %q", got)
	}

	if got := shorten("abc", 8); got != "abc" {
		t.Errorf("a short string is returned whole: %q", got)
	}
}

func TestMissingRepo(t *testing.T) {
	t.Parallel()

	if reason := missingRepo(gitx.Repo{Dir: t.TempDir() + "/nope"}); !strings.Contains(reason, "not cloned") {
		t.Errorf("got %q", reason)
	}

	if reason := missingRepo(gitx.Repo{Dir: t.TempDir()}); !strings.Contains(reason, "not a git repository") {
		t.Errorf("got %q", reason)
	}

	f := newFixture(t)
	if reason := missingRepo(gitx.Repo{Dir: f.parent}); reason != "" {
		t.Errorf("a real repository is not missing: %q", reason)
	}
}

// The wrapped git error leads with "exit status 128", which explains
// nothing; the fatal: line buried in the output is the actual reason.
func TestGitReason(t *testing.T) {
	t.Parallel()

	err := errors.New("git fetch --prune --tags origin: exit status 128\n" +
		"fatal: unable to access 'https://x/': Could not resolve host: x")
	if got := gitReason(err); got != "fatal: unable to access 'https://x/': Could not resolve host: x" {
		t.Errorf("got %q", got)
	}

	// The push summary says little; the rejected ref is the actual reason.
	err = errors.New("git push origin release-0.2: exit status 1\n" +
		"To git.example.com:group/app.git\n" +
		" ! [rejected]        0.1.3 -> 0.1.3 (already exists)\n" +
		"error: failed to push some refs to 'git.example.com:group/app.git'")
	if got := gitReason(err); got != "! [rejected]        0.1.3 -> 0.1.3 (already exists)" {
		t.Errorf("got %q", got)
	}

	// No fatal line — the first line is still better than nothing.
	if got := gitReason(errors.New("plain failure")); got != "plain failure" {
		t.Errorf("got %q", got)
	}
}

func TestPlanLinesMentionTheReview(t *testing.T) {
	t.Parallel()

	p := &config.Project{Name: "api", CI: config.CINone}
	shown := &run.Ctx{Cfg: &config.Config{}}

	lines := commitPlanLines(shown, p, "release-1.0", "files changed", "ci/AB-0000: update changelog")
	if !strings.Contains(lines[0], "diff shown first") {
		t.Errorf("got %q", lines[0])
	}

	if !strings.Contains(lines[1], `message: "ci/AB-0000: update changelog"`) {
		t.Errorf("the plan should name the commit message: %q", lines[1])
	}

	if got := pushPlanLine(shown, p, "release-1.0"); !strings.Contains(got, "review the picked commits") {
		t.Errorf("got %q", got)
	}

	off := false
	quiet := &run.Ctx{Cfg: &config.Config{Diff: &off}}

	if got := commitPlanLines(quiet, p, "release-1.0", "files changed", "msg")[0]; strings.Contains(got, "diff") {
		t.Errorf("with the review off the line should not mention it: %q", got)
	}

	if got := pushPlanLine(quiet, p, "release-1.0"); got != "push to origin/release-1.0" {
		t.Errorf("got %q", got)
	}
}

// A project with ci set tells the operator in the plan that the push is not
// the end of the step, and --no-ci silences exactly that.
func TestPlanLinesMentionTheCIWatch(t *testing.T) {
	t.Parallel()

	p := &config.Project{Name: "api", CI: config.CIGitLab}
	rctx := &run.Ctx{Cfg: &config.Config{}}

	if got := pushPlanLine(rctx, p, "release-1.0"); !strings.Contains(got, "watch the gitlab pipeline") {
		t.Errorf("got %q", got)
	}

	rctx.NoCI = true

	if got := pushPlanLine(rctx, p, "release-1.0"); strings.Contains(got, "pipeline") {
		t.Errorf("--no-ci must silence the watch: %q", got)
	}
}

// The pager is for bodies worth scrolling. A one-line tag message is not one:
// paging it would put a screen in front of a single sentence.
func TestPageTextSkipsASingleLine(t *testing.T) {
	t.Parallel()

	rctx := &run.Ctx{Cfg: &config.Config{}}

	for _, text := range []string{"", "2026.06 RC", "2026.06 RC\n"} {
		if pageText(rctx, text) {
			t.Errorf("%q should not be paged", text)
		}
	}
}

// The tag message is painted for reading only: the text pushed with the tag is
// the plain one, so no escape sequence may reach the value itself.
//
//nolint:paralleltest // flips the process-wide colour switch
func TestHighlightMessage(t *testing.T) {
	run.SetColor(true)
	defer run.SetColor(false)

	msg := "0.2.0-rc.0 2026.06-RC\n\n4 commit(s):\n0f9f759 feat/AB-9400: update"

	got := highlightMessage(msg)
	if !strings.Contains(got, "\x1b[1m0.2.0-rc.0 2026.06-RC") {
		t.Errorf("the subject should stand out: %q", got)
	}

	if !strings.Contains(got, "\x1b[33m0f9f759\x1b[0m feat/AB-9400: update") {
		t.Errorf("hashes should be yellow, the way git prints them: %q", got)
	}

	// The subject alone is the common case and must not gain a stray newline.
	if got := highlightMessage("release 1.2.3"); got != "\x1b[1mrelease 1.2.3\x1b[0m" {
		t.Errorf("got %q", got)
	}

	run.SetColor(false)

	if got := highlightMessage(msg); got != msg {
		t.Errorf("without colour the message is untouched: %q", got)
	}
}

//nolint:paralleltest // captures os.Stdout, which is process-wide
func TestPrintCapped(t *testing.T) {
	rctx := &run.Ctx{Cfg: &config.Config{}}

	short := captureOutput(t, func() { printCapped(rctx, "a\nb\nc", "more") })
	if short != "a\nb\nc\n" {
		t.Errorf("short text should print whole, got %q", short)
	}

	long := strings.Repeat("line\n", 220)

	out := captureOutput(t, func() { printCapped(rctx, long, "git -C /repo diff") })
	if strings.Count(out, "line") <= 200 {
		t.Error("the preview should print up to the cap")
	}

	if !strings.Contains(out, "more lines (diff_lines: 200), see them with: git -C /repo diff") {
		t.Errorf("the truncation notice should say how to see the rest: %q", out[len(out)-140:])
	}

	// A larger cap prints more of it.
	wider := 500
	rctx.Cfg.DiffLines = &wider

	out = captureOutput(t, func() { printCapped(rctx, long, "more") })
	if strings.Contains(out, "more lines") {
		t.Error("220 lines fit under a cap of 500")
	}

	// diff_lines: 0 prints everything, which a changelog commit usually wants.
	unlimited := 0
	rctx.Cfg.DiffLines = &unlimited

	out = captureOutput(t, func() { printCapped(rctx, strings.Repeat("x\n", 5000), "more") })
	if strings.Contains(out, "more lines") {
		t.Error("diff_lines: 0 should cut nothing")
	}
}

func TestSortedKeysAndEnvPairs(t *testing.T) {
	t.Parallel()

	env := map[string]string{"B": "2", "A": "1", "C": "3"}

	keys := sortedKeys(env)
	if strings.Join(keys, "") != "ABC" {
		t.Errorf("keys are not sorted: %v", keys)
	}

	pairs := envPairs(env)
	if strings.Join(pairs, " ") != "A=1 B=2 C=3" {
		t.Errorf("pairs = %v", pairs)
	}
}

// The changelog commands and their environment take the same placeholders,
// and {rt} must point at this binary so the built-in generator is reachable.
func TestChangelogCommandsAndEnvExpansion(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		ChangelogCmds: []string{"{rt} changelog gen --header '# CHANGELOG of {project}' > out.md"},
		ChangelogEnv:  map[string]string{"GIT_CLIFF__CHANGELOG__HEADER": "# Changelog of {project} on {branch}"},
	}
	rctx := &run.Ctx{Cfg: cfg}
	project := &config.Project{Name: "api"}

	cmds := changelogCmds(rctx, project, "develop")
	if len(cmds) != 1 {
		t.Fatalf("got %v", cmds)
	}

	if !strings.Contains(cmds[0], "# CHANGELOG of api") {
		t.Errorf("{project} was not expanded: %q", cmds[0])
	}

	self, err := os.Executable()
	if err == nil && !strings.HasPrefix(cmds[0], self) {
		t.Errorf("{rt} should expand to this binary (%s): %q", self, cmds[0])
	}

	env := changelogEnv(rctx, project, "develop")
	if env["GIT_CLIFF__CHANGELOG__HEADER"] != "# Changelog of api on develop" {
		t.Errorf("env not expanded: %q", env["GIT_CLIFF__CHANGELOG__HEADER"])
	}

	// Expansion must not write back into the config.
	if !strings.Contains(cfg.ChangelogEnv["GIT_CLIFF__CHANGELOG__HEADER"], "{project}") {
		t.Error("the config map was mutated by expansion")
	}
}

func TestEmptySelectionExplainsWhy(t *testing.T) {
	t.Parallel()

	project := &config.Project{Name: "api", DevBranch: "develop"}

	none := mustFilter(t, nil, nil)
	if got := emptySelection(project, "release-1.0", none, 0).Error(); !strings.Contains(
		got,
		"already has everything",
	) {
		t.Errorf("got %q", got)
	}

	filter := mustFilter(t, []string{"AB-1"}, nil)

	got := emptySelection(project, "release-1.0", filter, 3).Error()
	if !strings.Contains(got, "all 3 commit(s)") || !strings.Contains(got, "already in release-1.0") {
		t.Errorf("an all-already-picked selection must say so: %q", got)
	}

	got = emptySelection(project, "release-1.0", filter, 0).Error()
	if !strings.Contains(got, "nothing in develop is selected") {
		t.Errorf("an unmatched filter must say so: %q", got)
	}
}

// A negative flag must disable, not enable: returning a pointer to the flag
// variable itself would hand back true for --no-diff and --no-fetch.
func TestOverride(t *testing.T) {
	t.Parallel()

	if got := override(false, false); got != nil {
		t.Errorf("no flag means no override, got %v", *got)
	}

	got := override(true, false)
	if got == nil || !*got {
		t.Errorf("the positive flag must enable, got %v", got)
	}

	got = override(false, true)
	if got == nil || *got {
		t.Errorf("the negative flag must disable, got %v", got)
	}
}

func TestRejectBothFlags(t *testing.T) {
	t.Parallel()

	if err := rejectBothFlags(true, true, "--diff", "--no-diff"); err == nil {
		t.Error("both switches at once is a contradiction")
	}

	for _, pair := range [][2]bool{{false, false}, {true, false}, {false, true}} {
		if err := rejectBothFlags(pair[0], pair[1], "--diff", "--no-diff"); err != nil {
			t.Errorf("%v should be allowed: %v", pair, err)
		}
	}
}

// Every template sees the product version, and the tag messages additionally
// see the computed tag.
func TestMessageVarsCarryTheProductVersion(t *testing.T) {
	t.Parallel()

	rctx := &run.Ctx{Cfg: &config.Config{ProductVersion: "2026.06"}}
	project := &config.Project{Name: "api"}

	vars := messageVars(rctx, project, "release-6.8")

	got := expand("freeze {branch} of {project} for {product_version}", vars)
	if got != "freeze release-6.8 of api for 2026.06" {
		t.Errorf("got %q", got)
	}

	// {version} is not among them: only the tag commands add it.
	if got := expand("{version}", vars); got != "{version}" {
		t.Errorf("commit messages have no version: %q", got)
	}
}

// A dev config has no business freezing dependencies for a release, so it can
// say so once instead of relying on everyone remembering which config is loaded.
func TestDisabledCommands(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	body := "disable:\n" +
		"  - \"deps freeze\"\n" +
		"projects:\n" +
		"  - name: api\n" +
		"    git: git@example.com:group/api.git\n" +
		"    project_dir: " + dir + "\n"

	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	run := func(args ...string) error {
		root := NewRoot()
		root.SetOut(io.Discard)
		root.SetErr(io.Discard)
		root.SetArgs(append([]string{"-c", path}, args...))

		return root.Execute()
	}

	err := run("deps", "freeze", "--dry-run", "-y")
	if err == nil || !strings.Contains(err.Error(), "deps freeze is disabled") {
		t.Fatalf("got %v", err)
	}

	// Only what the list names: its sibling still runs.
	if err := run("deps", "submodules", "--dry-run", "-y"); err != nil {
		t.Errorf("deps submodules is not disabled: %v", err)
	}

	// A prohibition that names no real command is a typo, and typos here mean
	// the guard silently never applies.
	body = strings.Replace(body, "deps freeze", "deps frezee", 1)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := run("repo", "status"); err == nil || !strings.Contains(err.Error(), "is not a command") {
		t.Errorf("got %v", err)
	}
}

// help --all is what makes twelve commands and their flags reviewable at once,
// so it has to cover every leaf and print the flags, not just the names.
func TestHelpAllCoversTheTree(t *testing.T) {
	t.Parallel()

	root := NewRoot()

	var out strings.Builder

	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"help", "--all"})

	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}

	text := out.String()
	for _, want := range []string{
		"rt repo clean", "rt deps submodules", "rt git cherry-pick", "rt changelog gen",
		"--allow-dirty", "--untracked", "--task", "--keep-going",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("%q is missing from the dump", want)
		}
	}

	// The generated completion branch is noise here.
	if strings.Contains(text, "rt completion bash") {
		t.Error("the completion tree should be left out")
	}

	// A single command still works the ordinary way. A fresh root, because a
	// real run is a fresh process and cobra keeps parsed flags on the command.
	single := NewRoot()

	out.Reset()
	single.SetOut(&out)
	single.SetArgs([]string{"help", "repo", "clean"})

	if err := single.Execute(); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(out.String(), "--untracked") || strings.Contains(out.String(), "cherry-pick") {
		t.Errorf("help for one command should be just that one:\n%s", out.String())
	}
}

// The review body goes through a pager on a terminal; in a pipe — which is
// where tests and CI live — it is printed and the cap applies.
func TestPagerIsSkippedWithoutATerminal(t *testing.T) {
	t.Parallel()

	rctx := &run.Ctx{Cfg: &config.Config{}}

	if pageText(rctx, "some text") {
		t.Error("a pipe must not be handed to a pager")
	}

	// An empty pager setting disables it even on a terminal.
	off := ""
	rctx.Cfg.Pager = &off

	if got := rctx.Cfg.PagerCommand(); got != "" {
		t.Errorf("pager = %q, want it disabled", got)
	}
}
