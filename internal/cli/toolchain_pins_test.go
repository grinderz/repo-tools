package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grinderz/repo-tools/internal/config"
	"github.com/grinderz/repo-tools/internal/gitx"
	"github.com/grinderz/repo-tools/internal/run"
)

// dirFiles lists a directory the way git lists a working tree, for pins
// outside a repository.
func dirFiles(t *testing.T, dir string) []string {
	t.Helper()

	var paths []string

	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}

		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return fmt.Errorf("rel: %w", err)
		}

		paths = append(paths, filepath.ToSlash(rel))

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	return paths
}

// A pin finds its line, reports the version there against the wanted one
// with the line number, rewrites the capture group alone when asked, every
// occurrence; a missing file is skipped, a required pattern that matches
// nothing is an error, an optional one is not.
func TestApplyPins(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeFile(t, dir, ".gitlab-ci.yml", "variables:\n  GO_VERSION: \"1.25\"\n  UV_VERSION: \"0.12\"\n")
	writeFile(t, dir, "ci.yml", "- uses: actions/checkout@v4\n- uses: actions/checkout@v4\n")
	writeFile(t, dir, "go.mod", "module m\n\ngo 1.25.0\n")
	writeFile(t, dir, "win.mod", "module m\r\n\r\ngo 1.25.0\r\n")

	for _, svc := range []string{"svc-api", "svc-web"} {
		if err := os.Mkdir(filepath.Join(dir, svc), 0o750); err != nil {
			t.Fatal(err)
		}

		writeFile(t, dir, svc+"/Dockerfile", "RUN go install example.com/gen@v1.0.0\n")
	}

	vars := map[string]string{"go_minor": "1.26", "go": "1.26.0", "uv": "0.12", "checkout": "v7", "gen": "v1.2.0"}
	pins := []config.Pin{
		{File: "svc-*/Dockerfile", Match: `example.com/gen@(\S+)`, Value: "{gen}"},
		{File: "job-*/Dockerfile", Match: `example.com/gen@(\S+)`, Value: "{gen}"}, // a glob no file matches
		{File: ".gitlab-ci.yml", Match: `^  GO_VERSION: "(.+)"$`, Value: "{go_minor}"},
		{File: ".gitlab-ci.yml", Match: `^  UV_VERSION: "(.+)"$`, Value: "{uv}"},
		{File: "ci.yml", Match: `actions/checkout@(\S+)`, Value: "{checkout}"},
		{File: "go.mod", Match: `^go (.+)$`, Value: "{go}"},
		{File: "go.mod", Match: `^toolchain go(.+)$`, Value: "{go}", Optional: true},
		{File: "pyproject.toml", Match: `^requires-python = ">=(.+?),`, Value: "{go}"},
		{File: "win.mod", Match: `^go (.+)$`, Value: "{go}"}, // CRLF: .+ reaches the \r, which stays the line's
	}

	hits, applied, err := applyPins(dir, dirFiles(t, dir), pins, vars, false)
	if err != nil {
		t.Fatal(err)
	}

	if applied != 6 {
		t.Errorf("applied = %d, want the six pins that found a file", applied)
	}

	want := []string{
		"svc-api/Dockerfile:1 gen: v1.0.0 -> v1.2.0",
		"svc-web/Dockerfile:1 gen: v1.0.0 -> v1.2.0",
		".gitlab-ci.yml:2 go_minor: 1.25 -> 1.26",
		"ci.yml:1 checkout: v4 -> v7",
		"ci.yml:2 checkout: v4 -> v7",
		"go.mod:3 go: 1.25.0 -> 1.26.0",
		"win.mod:3 go: 1.25.0 -> 1.26.0",
	}

	got := make([]string, 0, len(hits))
	for _, h := range hits {
		got = append(got, h.String())
	}

	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("hits = %q, want %q", got, want)
	}

	if body, _ := os.ReadFile(filepath.Join(dir, "go.mod")); !strings.Contains(string(body), "go 1.25.0") {
		t.Error("a check must not write")
	}

	if _, _, err := applyPins(dir, dirFiles(t, dir), pins, vars, true); err != nil {
		t.Fatal(err)
	}

	for file, want := range map[string]string{
		".gitlab-ci.yml":     "variables:\n  GO_VERSION: \"1.26\"\n  UV_VERSION: \"0.12\"\n",
		"ci.yml":             "- uses: actions/checkout@v7\n- uses: actions/checkout@v7\n",
		"win.mod":            "module m\r\n\r\ngo 1.26.0\r\n",
		"go.mod":             "module m\n\ngo 1.26.0\n",
		"svc-web/Dockerfile": "RUN go install example.com/gen@v1.2.0\n",
	} {
		if body, _ := os.ReadFile(filepath.Join(dir, file)); string(body) != want {
			t.Errorf("%s = %q, want %q", file, body, want)
		}
	}

	if hits, _, _ := applyPins(dir, dirFiles(t, dir), pins, vars, false); len(hits) != 0 {
		t.Errorf("after the write everything is on the versions: %v", hits)
	}

	// A required pin that matches nothing in a file the project has is the
	// silent no-op made loud; a group that takes no part in the match is
	// the same thing, not a panic.
	for _, pin := range []config.Pin{
		{File: "go.mod", Match: `^toolchain go(.+)$`, Value: "{go}"},
		{File: "go.mod", Match: `^module m(\S+)?$`, Value: "{go}"},
	} {
		_, _, err = applyPins(dir, dirFiles(t, dir), []config.Pin{pin}, vars, false)
		if !errors.Is(err, errPinNotFound) || !strings.Contains(err.Error(), "go.mod") {
			t.Errorf("%s: err = %v, want the pin named as matching nothing", pin.Match, err)
		}
	}
}

// toolchainPinsCtx holds a go project to one CI variable and the go directive
// through pins, with no shell command at all.
func toolchainPinsCtx(p *config.Project) *run.Ctx {
	rctx := ctxFor(p)
	rctx.Cfg.Toolchain = map[string]string{"go": "1.26.0"}
	rctx.Cfg.Cmds.Deps = map[string][]string{"go": {"true"}}
	rctx.Cfg.Pins = []config.Pin{
		{File: ".gitlab-ci.yml", Match: `^  GO_VERSION: "(.+)"$`, Value: "{go_minor}", Kinds: []string{"go"}},
		{File: "go.mod", Match: `^go (.+)$`, Value: "{go}"},
		{File: "Dockerfile", Match: `^ARG GO_VERSION=(.+)$`, Value: "{go_minor}"},
	}
	rctx.Cfg.ToolchainCommitMessage = "build/AB-0000: bump go to {go}"
	p.Deps = "go"

	return rctx
}

// The plan reads the pins' files on the branch and shows each line that
// moves; the bump rewrites them and commits; deps check reads them and
// says stale before and current after.
//
//nolint:paralleltest // captures os.Stdout, which is process-wide
func TestToolchainPinsPlanBumpAndCheck(t *testing.T) {
	f := newFixture(t)
	onDevelop(t, f)

	writeFile(t, f.parent, ".gitlab-ci.yml", "variables:\n  GO_VERSION: \"1.25\"\n")
	writeFile(t, f.parent, "go.mod", "module m\n\ngo 1.25.0\n")
	commit(t, f.parent, "parent: pinned files")

	p := f.project(t)
	rctx := toolchainPinsCtx(p)

	st, err := planToolchain(rctx, p, toolchainOpts{NoCommit: true})
	if err != nil || st.Skip {
		t.Fatalf("step = %+v, %v", st, err)
	}

	plan := strings.Join(st.Plan, "\n")
	for _, want := range []string{
		"toolchain: go 1.26.0",
		"pin .gitlab-ci.yml:2 go_minor: 1.25 -> 1.26",
		"pin go.mod:3 go: 1.25.0 -> 1.26.0",
	} {
		if !strings.Contains(plan, want) {
			t.Errorf("plan lacks %q:\n%s", want, plan)
		}
	}

	if strings.Contains(plan, "Dockerfile") {
		t.Errorf("a pin whose file the branch lacks is not planned:\n%s", plan)
	}

	out := captureOutput(t, func() {
		if ok, _ := depsCheckProject(rctx, p); ok {
			t.Error("behind the configured go, deps check should say stale")
		}
	})

	if !strings.Contains(out, ".gitlab-ci.yml:2 go_minor: 1.25 -> 1.26") ||
		!strings.Contains(out, "go.mod:3 go: 1.25.0 -> 1.26.0") {
		t.Errorf("deps check output:\n%s", out)
	}

	captureOutput(t, func() {
		if err := bumpToolchain(rctx, p, "develop", toolchainOpts{NoCommit: true}); err != nil {
			t.Errorf("bump: %v", err)
		}
	})

	if body, _ := os.ReadFile(filepath.Join(f.parent, ".gitlab-ci.yml")); !strings.Contains(string(body), `"1.26"`) {
		t.Errorf("ci file = %q", body)
	}

	out = captureOutput(t, func() {
		if ok, ran := depsCheckProject(rctx, p); !ok || !ran {
			t.Errorf("on the versions, deps check should say current: ok %v ran %v", ok, ran)
		}
	})

	if !strings.Contains(out, "pins: 2 on the configured versions") {
		t.Errorf("deps check output:\n%s", out)
	}
}

// A project with none of the pinned files and no commands is skipped, and
// deps check has nothing to say about it.
//
//nolint:paralleltest // captures os.Stdout, which is process-wide
func TestToolchainPinsSkipWithoutFiles(t *testing.T) {
	f := newFixture(t)
	onDevelop(t, f)

	p := f.project(t)
	rctx := toolchainPinsCtx(p)

	if st, _ := planToolchain(rctx, p, toolchainOpts{}); !st.Skip ||
		!strings.Contains(st.Warn, "no toolchain pins or commands for deps kind go") {
		t.Errorf("step = %+v", st)
	}

	out := captureOutput(t, func() {
		if _, ran := depsCheckProject(rctx, p); ran {
			t.Error("nothing to check, nothing ran")
		}
	})

	if !strings.Contains(out, "none of the pinned files is here") {
		t.Errorf("deps check output:\n%s", out)
	}
}

// A check reports every pin that found no line, not the first alone, and
// the hits of the pins after it; a write stops at the first.
func TestApplyPinsCheckGoesOn(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeFile(t, dir, "go.mod", "module m\n\ngo 1.25.0\n")
	writeFile(t, dir, "ci.yml", "GO_VERSION: 1.25\n")

	pins := []config.Pin{
		{File: "go.mod", Match: `^toolchain go(.+)$`, Value: "{go}"},
		{File: "ci.yml", Match: `^GO_VERSION: (.+)$`, Value: "{go_minor}"},
		{File: "ci.yml", Match: `^UV_VERSION: (.+)$`, Value: "{uv}"},
	}
	vars := map[string]string{"go": "1.26.0", "go_minor": "1.26", "uv": "0.12"}

	hits, applied, err := applyPins(dir, dirFiles(t, dir), pins, vars, false)
	if err == nil || strings.Count(err.Error(), "matches nothing") != 2 || applied != 1 ||
		len(hits) != 1 || hits[0].String() != "ci.yml:1 go_minor: 1.25 -> 1.26" {
		t.Errorf("hits = %v, applied = %d, err = %v", hits, applied, err)
	}

	if _, _, err := applyPins(dir, dirFiles(t, dir), pins, vars, true); err == nil ||
		strings.Count(err.Error(), "matches nothing") != 1 {
		t.Errorf("a write stops at the first: %v", err)
	}
}

// The plan reads the branch's files: a line that moves is shown with its
// current value, one that stays with the value alone, a required line
// that is missing is named.
func TestPinPlanLines(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	onDevelop(t, f)
	writeFile(t, f.parent, "go.mod", "module m\n\ngo 1.25.0\n")
	writeFile(t, f.parent, "ci.yml", "GO_VERSION: 1.26\nUV_VERSION: 0.11\n")
	commit(t, f.parent, "parent: versions")

	// the checkout moves on: the plan has to read the branch, not it
	writeFile(t, f.parent, "go.mod", "module m\n\ngo 1.27.0\n")

	r := gitx.Repo{Dir: f.parent}
	pins := []config.Pin{
		{File: "go.mod", Match: `^go (.+)$`, Value: "{go}"},
		{File: "ci.yml", Match: `^GO_VERSION: (.+)$`, Value: "{go_minor}"},
		{File: "ci.yml", Match: `^UV_VERSION: (.+)$`, Value: "{uv}"},
		{File: "ci.yml", Match: `^PY_VERSION: (.+)$`, Value: "{uv}"},
		{File: "go.mod", Match: `^toolchain go(.+)$`, Value: "{go}", Optional: true},
	}
	vars := map[string]string{"go": "1.26.0", "go_minor": "1.26", "uv": "0.12"}

	present, paths, err := pinsAt(r, "develop", pins)
	if err != nil || len(present) != 5 {
		t.Fatalf("pinsAt: %d pins, %v", len(present), err)
	}

	got := strings.Join(pinPlanLines(r, "develop", paths, present, vars), "\n")

	want := "pin go.mod:3 go: 1.25.0 -> 1.26.0\n" +
		"pin ci.yml:1 go_minor: 1.26\n" +
		"pin ci.yml:2 uv: 0.11 -> 0.12\n" +
		"pin ci.yml: \"^PY_VERSION: (.+)$\" matches nothing, the bump would stop here"
	if got != want {
		t.Errorf("plan:\n%s\nwant:\n%s", got, want)
	}
}
