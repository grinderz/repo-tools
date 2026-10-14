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

// toolchainCtx is a context whose config has a go kind with toolchain
// commands that write the versions into a file, the way sed into go.mod
// would.
func toolchainCtx(p *config.Project) *run.Ctx {
	rctx := ctxFor(p)
	rctx.Cfg.Toolchain = map[string]string{"go": "1.26", "go_toolchain": "1.26.2", "python": "3.14"}
	rctx.Cfg.Cmds.Deps = map[string][]string{"go": {"true"}}
	rctx.Cfg.Cmds.Toolchain = map[string][]string{"go": {"echo {go_minor} {go_toolchain} > toolchain.txt"}}
	rctx.Cfg.Cmds.ToolchainCheck = map[string][]string{"go": {"grep -q 1.26.2 toolchain.txt"}}
	rctx.Cfg.ToolchainCommitMessage = "build/AB-0000: bump go to {go_toolchain}"
	p.Deps = "go"

	return rctx
}

// The plan names the versions the commands use and shows them expanded; a
// project with no commands for its kind is skipped with the reason.
func TestPlanToolchain(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	onDevelop(t, f)

	p := f.project(t)
	rctx := toolchainCtx(p)

	st, err := planToolchain(rctx, p, toolchainOpts{})
	if err != nil || st.Skip {
		t.Fatalf("step = %+v, %v", st, err)
	}

	plan := strings.Join(st.Plan, "\n")
	for _, want := range []string{
		"checkout develop",
		"toolchain: go 1.26, go_toolchain 1.26.2",
		"toolchain (go): echo 1.26 1.26.2 > toolchain.txt",
		`message: "build/AB-0000: bump go to 1.26.2"`,
	} {
		if !strings.Contains(plan, want) {
			t.Errorf("plan lacks %q:\n%s", want, plan)
		}
	}

	if strings.Contains(plan, "python") {
		t.Errorf("python is not in the commands and should not be in the summary:\n%s", plan)
	}

	p.Deps = "uv"

	st, _ = planToolchain(rctx, p, toolchainOpts{})
	if !st.Skip || !strings.Contains(st.Warn, "no toolchain pins or commands for deps kind uv") {
		t.Errorf("a kind without commands should be skipped: %+v", st)
	}
}

// The commands run on the target branch and their result is committed; a
// rerun finds nothing to commit; --no-commit leaves the change in the tree.
//
//nolint:paralleltest // captures os.Stdout, which is process-wide
func TestBumpToolchainCommitsWhatChanged(t *testing.T) {
	f := newFixture(t)
	onDevelop(t, f)

	p := f.project(t)
	rctx := toolchainCtx(p)

	captureOutput(t, func() {
		// No origin, so the push at the end fails; the commit before it is
		// what this test is about.
		_ = bumpToolchain(rctx, p, "develop", toolchainOpts{})
	})

	if subject := git(t, f.parent, "log", "-1", "--format=%s"); subject != "build/AB-0000: bump go to 1.26.2" {
		t.Errorf("commit subject = %q", subject)
	}

	body, _ := os.ReadFile(filepath.Join(f.parent, "toolchain.txt"))
	if string(body) != "1.26 1.26.2\n" {
		t.Errorf("toolchain.txt = %q", body)
	}

	// --no-commit with another version leaves the change in the tree.
	other := newFixture(t)
	onDevelop(t, other)

	otherProject := other.project(t)
	otherCtx := toolchainCtx(otherProject)

	captureOutput(t, func() {
		if err := bumpToolchain(otherCtx, otherProject, "develop", toolchainOpts{NoCommit: true}); err != nil {
			t.Errorf("no-commit run: %v", err)
		}
	})

	if status := git(t, other.parent, "status", "--porcelain"); !strings.Contains(status, "toolchain.txt") {
		t.Errorf("--no-commit should leave the change in the tree, status:\n%s", status)
	}

	// A second --no-commit run finds the tree already there and says so in
	// the toolchain's words.
	git(t, other.parent, "add", "-A")
	commit(t, other.parent, "parent: toolchain")

	out := captureOutput(t, func() {
		if err := bumpToolchain(otherCtx, otherProject, "develop", toolchainOpts{NoCommit: true}); err != nil {
			t.Errorf("second no-commit run: %v", err)
		}
	})
	if !strings.Contains(out, "already on the configured toolchain") {
		t.Errorf("nothing to change:\n%s", out)
	}
}

// deps check runs the toolchain checks too: a project behind the configured
// version is stale.
//
//nolint:paralleltest // captures os.Stdout, which is process-wide
func TestDepsCheckRunsTheToolchainChecks(t *testing.T) {
	f := newFixture(t)
	onDevelop(t, f)

	p := f.project(t)
	rctx := toolchainCtx(p)
	r := gitx.Repo{Dir: f.parent}

	writeFile(t, f.parent, "toolchain.txt", "1.25 1.25.14\n")

	var ok, ran bool

	out := captureOutput(t, func() { ok, ran = depsCheckProject(rctx, p) })
	if ok || !ran || !strings.Contains(out, "toolchain check: grep -q 1.26.2 toolchain.txt") {
		t.Errorf("an old toolchain should be stale: ok=%v ran=%v\n%s", ok, ran, out)
	}

	writeFile(t, f.parent, "toolchain.txt", "1.26 1.26.2\n")

	if tcOK, tcRan := toolchainCheckProject(rctx, r, p); !tcOK || !tcRan {
		t.Errorf("the configured toolchain should pass: ok=%v ran=%v", tcOK, tcRan)
	}
}

// repo check reports what deps toolchain and deps renovate would trip over:
// a program not on PATH, a missing or broken renovate config.
//
//nolint:paralleltest // fakeGlab sets PATH
func TestCheckToolchainAndRenovate(t *testing.T) {
	fakeGlab(t)

	f, rctx, p := renovateFixture(t, false)
	r := gitx.Repo{Dir: f.parent}

	rctx.Cfg.Cmds.Deps = map[string][]string{"go": {"true"}}
	rctx.Cfg.Cmds.Toolchain = map[string][]string{"go": {"definitely-not-a-real-program {go}"}}
	rctx.Cfg.Toolchain = map[string]string{"go": "1.26"}
	p.Deps = "go"

	if issues := checkToolchain(rctx, p); len(issues) != 1 ||
		!strings.Contains(issues[0], "definitely-not-a-real-program") {
		t.Errorf("toolchain issues = %v", issues)
	}

	if issues := checkRenovateConfig(rctx, r, p); len(issues) != 0 {
		t.Errorf("without renovate: true a missing config is no finding: %v", issues)
	}

	required := true
	p.Renovate = &required

	if issues := checkRenovateConfig(rctx, r, p); len(issues) != 1 ||
		!strings.Contains(issues[0], "renovate: true but no renovate.json on develop") {
		t.Errorf("missing config: %v", issues)
	}

	writeFile(t, f.parent, "renovate.json", "{not json\n")
	commit(t, f.parent, "parent: broken renovate config")
	git(t, f.parent, "push", "--quiet", "origin", "develop")
	git(t, f.parent, "fetch", "--quiet", "origin")

	if issues := checkRenovateConfig(rctx, r, p); len(issues) != 1 ||
		!strings.Contains(issues[0], "is not valid JSON") {
		t.Errorf("broken config: %v", issues)
	}

	writeFile(t, f.parent, "renovate.json", `{"separateMajorMinor": false}`+"\n")
	commit(t, f.parent, "parent: a level key of its own")
	git(t, f.parent, "push", "--quiet", "origin", "develop")
	git(t, f.parent, "fetch", "--quiet", "origin")

	if issues := checkRenovateConfig(rctx, r, p); len(issues) != 1 ||
		!strings.Contains(
			issues[0],
			"renovate: renovate.json on develop sets separateMajorMinor, which --update owns",
		) {
		t.Errorf("a level key in the file: %v", issues)
	}

	off := false
	p.Renovate = &off

	if issues := checkRenovateConfig(rctx, r, p); len(issues) != 0 {
		t.Errorf("renovate: false should be silent: %v", issues)
	}

	if issues := checkRenovate(rctx, []*config.Project{p}); len(issues) != 0 {
		t.Errorf("no project asked for renovate, nothing to check: %v", issues)
	}

	p.Renovate = &required
	rctx.Cfg.Renovate = config.Renovate{Docker: "definitely-not-docker", TokenEnv: "RT_TEST_NO_TOKEN"}

	issues := checkRenovate(rctx, []*config.Project{p})
	if len(issues) != 1 || !strings.Contains(issues[0], "definitely-not-docker is not in PATH") {
		t.Errorf("renovate issues = %v", issues)
	}
}

// The cache directory check writes nothing: a missing directory under a
// writable parent is fine and stays missing, a file in its place and a
// parent nobody can write to are reported.
func TestCheckCacheDir(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	missing := filepath.Join(dir, "a", "b", "cache")

	if issue := checkCacheDir(missing); issue != "" {
		t.Errorf("missing under a writable parent: %q", issue)
	}

	if _, err := os.Stat(filepath.Join(dir, "a")); err == nil {
		t.Error("the check must not create the directory")
	}

	if issue := checkCacheDir(""); issue != "" {
		t.Errorf("no cache_dir, nothing to check: %q", issue)
	}

	file := filepath.Join(dir, "file")
	writeFile(t, dir, "file", "x")

	if issue := checkCacheDir(file); !strings.Contains(issue, "is not a directory") {
		t.Errorf("a file: %q", issue)
	}

	if issue := checkCacheDir(filepath.Join(file, "cache")); !strings.Contains(issue, "is not a directory") {
		t.Errorf("under a file: %q", issue)
	}

	if os.Getuid() == 0 {
		return // root writes anywhere
	}

	locked := filepath.Join(dir, "locked")
	if err := os.Mkdir(locked, 0o500); err != nil {
		t.Fatal(err)
	}

	if issue := checkCacheDir(filepath.Join(locked, "cache")); !strings.Contains(issue, "is not writable") {
		t.Errorf("under a read-only parent: %q", issue)
	}

	// A parent that cannot even be looked into is reported, not walked past
	// to a writable grandparent.
	sealed := filepath.Join(dir, "sealed")
	if err := os.Mkdir(sealed, 0o000); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = os.Chmod(sealed, 0o700) })

	if issue := checkCacheDir(filepath.Join(sealed, "deeper", "cache")); !strings.Contains(issue, "permission denied") {
		t.Errorf("under an unreadable parent: %q", issue)
	}
}

func TestMinorAndNextMinor(t *testing.T) {
	t.Parallel()

	cases := []struct{ version, minor, next string }{
		{"3.13", "3.13", "3.14"},
		{"1.25.10", "1.25", "1.26"},
		{"3.9", "3.9", "3.10"},
		{"4", "4", ""},
		{"3.14rc1", "3.14rc1", ""}, // no minor number to step
	}

	for _, tc := range cases {
		if got := config.MinorOf(tc.version); got != tc.minor {
			t.Errorf("MinorOf(%q) = %q, want %q", tc.version, got, tc.minor)
		}

		if got, _ := config.NextMinorOf(tc.version); got != tc.next {
			t.Errorf("NextMinorOf(%q) = %q, want %q", tc.version, got, tc.next)
		}
	}
}

// requires-python moves with the configured python: the lower bound to the
// new minor and an exact upper bound like <3.14 to the minor after it, while
// an open one like <4 stays — the command the README shows, run for real.
//
//nolint:paralleltest // captures os.Stdout, which is process-wide
func TestToolchainMovesRequiresPython(t *testing.T) {
	cases := []struct{ before, after string }{
		{`requires-python = ">=3.13,<3.14"`, `requires-python = ">=3.14,<3.15"`},
		{`requires-python = ">=3.13,<4"`, `requires-python = ">=3.14,<4"`},
		{`requires-python = ">=3.13"`, `requires-python = ">=3.14"`},
	}

	for _, tc := range cases {
		f := newFixture(t)
		onDevelop(t, f)
		writeFile(t, f.parent, "pyproject.toml", "[project]\n"+tc.before+"\n")
		commit(t, f.parent, "parent: pyproject")

		p := f.project(t)
		p.Deps = "uv"

		rctx := ctxFor(p)
		rctx.Cfg.Toolchain = map[string]string{"python": "3.14"}
		rctx.Cfg.Cmds.Deps = map[string][]string{"uv": {"true"}}
		rctx.Cfg.Cmds.Toolchain = map[string][]string{"uv": {
			`perl -pi -e 's/^(requires-python = ">=)\d+\.\d+/${1}{python_minor}/; ` +
				`s/^(requires-python = ">=[\d.]+,<)\d+\.\d+"/${1}{python_next_minor}"/' pyproject.toml`,
		}}

		captureOutput(t, func() {
			if err := bumpToolchain(rctx, p, "develop", toolchainOpts{NoCommit: true}); err != nil {
				t.Errorf("%s: %v", tc.before, err)
			}
		})

		body, _ := os.ReadFile(filepath.Join(f.parent, "pyproject.toml"))
		if got := strings.TrimSpace(strings.TrimPrefix(string(body), "[project]\n")); got != tc.after {
			t.Errorf("%s: got %s, want %s", tc.before, got, tc.after)
		}
	}
}

// A target branch nobody has is a skip with the reason, not a plan read
// off whatever is checked out.
func TestPlanToolchainSkipsAMissingBranch(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	p := f.project(t)
	rctx := toolchainCtx(p)
	p.ReleaseBranch = "release-9.9"

	st, err := planToolchain(rctx, p, toolchainOpts{})
	if err != nil || !st.Skip || st.Warn != "branch release-9.9 exists neither locally nor on origin" {
		t.Errorf("st = %+v, err = %v", st, err)
	}
}
