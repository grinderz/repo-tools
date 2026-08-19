package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grinderz/repo-tools/internal/config"
	"github.com/grinderz/repo-tools/internal/run"
)

// pinsCtx is the fixture as two projects: a consumer and the library it pins as
// a go module rather than as a submodule.
func pinsCtx(t *testing.T, f fixture) (*run.Ctx, *config.Project) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.yaml")
	body := "deps_cmds:\n" +
		"  go:\n" +
		"    - \"make deps.update.internal\"\n" +
		"deps_pins:\n" +
		"  go:\n" +
		"    file: Makefile\n" +
		"    var: GO_DEPS_UPDATE_INTERNAL\n" +
		"projects:\n" +
		"  - name: parent\n" +
		"    git: " + f.parent + "\n" +
		"    project_dir: " + f.parent + "\n" +
		"    deps: go\n" +
		"  - name: sub\n" +
		"    git: " + f.sub + "\n" +
		"    project_dir: " + f.sub + "\n"

	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}

	rctx := testCtx()
	rctx.Cfg = cfg

	return rctx, cfg.Projects[0]
}

func makefileWith(modules ...string) string {
	var text strings.Builder

	text.WriteString("GO_DEPS_UPDATE_INTERNAL := \\\n")

	for _, module := range modules {
		text.WriteString("\t" + module + "@release-1.0 \\\n")
	}

	text.WriteString("\nGO_MOD_ID := example.com/app\n")

	return text.String()
}

// A go project pins its libraries in a make variable, not in .gitmodules, so
// the freeze has to move those refs too — onto whatever branch the providing
// project works on in this config.
func TestRewritePinsFollowsTheProviderBranch(t *testing.T) {
	f := newFixture(t)
	rctx, _ := pinsCtx(t, f)

	updated, changes, ok, err := rewritePins(rctx, makefileWith(f.sub), "GO_DEPS_UPDATE_INTERNAL")
	if !ok || err != nil {
		t.Fatalf("ok=%v err=%v", ok, err)
	}

	if len(changes) != 1 || changes[0].From != "release-1.0" || changes[0].To != "develop" {
		t.Fatalf("changes = %v", changes)
	}

	if !strings.Contains(updated, f.sub+"@develop") {
		t.Errorf("the ref did not move:\n%s", updated)
	}

	// Everything outside the variable stays untouched, continuation and all.
	if !strings.Contains(updated, "GO_MOD_ID := example.com/app") {
		t.Errorf("the rest of the file changed:\n%s", updated)
	}

	// Running it twice must be a no-op, or every freeze would show a diff.
	if _, again, _, _ := rewritePins(rctx, updated, "GO_DEPS_UPDATE_INTERNAL"); len(again) != 0 {
		t.Errorf("already frozen, nothing to move: %v", again)
	}
}

func TestRewritePinsWithoutTheVariable(t *testing.T) {
	f := newFixture(t)
	rctx, _ := pinsCtx(t, f)

	text := "GO_MOD_ID := example.com/app\n"

	updated, _, ok, _ := rewritePins(rctx, text, "GO_DEPS_UPDATE_INTERNAL")
	if ok {
		t.Error("a file without the variable has no pins to freeze")
	}

	if updated != text {
		t.Errorf("the file must come back unchanged: %q", updated)
	}
}

// The plan reads the branch being frozen, and freezePins writes the checkout.
func TestPlanAndFreezePins(t *testing.T) {
	f := newFixture(t)
	rctx, p := pinsCtx(t, f)

	writeFile(t, f.parent, "Makefile", makefileWith(f.sub))
	commit(t, f.parent, "parent: add Makefile")

	plan, err := planPins(rctx, repoOf(p), p, "")
	if err != nil || len(plan) != 1 || !strings.Contains(plan[0], "@release-1.0 -> develop") {
		t.Fatalf("plan = %v, err = %v", plan, err)
	}

	if err := freezePins(rctx, repoOf(p), p); err != nil {
		t.Fatal(err)
	}

	body, err := os.ReadFile(filepath.Join(f.parent, "Makefile"))
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(body), f.sub+"@develop") {
		t.Errorf("Makefile was not rewritten:\n%s", body)
	}
}

// A branch that never had the file is a state to report, not a failure: an old
// release branch may predate the pinning convention entirely.
func TestPlanPinsWithoutTheFile(t *testing.T) {
	f := newFixture(t)
	rctx, p := pinsCtx(t, f)

	plan, err := planPins(rctx, repoOf(p), p, "")
	if err != nil || len(plan) != 1 || !strings.Contains(plan[0], "Makefile MISSING") {
		t.Fatalf("plan = %v, err = %v", plan, err)
	}

	if err := freezePins(rctx, repoOf(p), p); err != nil {
		t.Errorf("a missing file must not stop the freeze: %v", err)
	}
}

// deps_pins is keyed by deps kind, so a project of another language has none.
func TestPinsAreScopedToTheDepsKind(t *testing.T) {
	f := newFixture(t)
	rctx, p := pinsCtx(t, f)

	writeFile(t, f.parent, "Makefile", makefileWith(f.sub))

	p.Deps = config.DepsNone

	plan, err := planPins(rctx, repoOf(p), p, "")
	if plan != nil || err != nil {
		t.Errorf("nothing is pinned for this kind: %v %v", plan, err)
	}
}

// The variable lists internal dependencies, so a module this config knows
// nothing about means a missing project — and freezing the rest would ship a
// release with that one dependency still on its dev branch.
func TestPinsRefuseAnUnknownModule(t *testing.T) {
	f := newFixture(t)
	rctx, p := pinsCtx(t, f)

	text := makefileWith(f.sub, "git.example.com/group/stranger")

	if _, _, _, err := rewritePins(rctx, text, "GO_DEPS_UPDATE_INTERNAL"); err == nil ||
		!strings.Contains(err.Error(), "stranger") {
		t.Fatalf("err = %v", err)
	}

	writeFile(t, f.parent, "Makefile", text)

	if _, err := planPins(rctx, repoOf(p), p, ""); err == nil {
		t.Error("the plan must not describe a freeze it cannot finish")
	}

	if err := freezePins(rctx, repoOf(p), p); err == nil {
		t.Error("the freeze must stop")
	}

	// Nothing was written on the way out.
	body, err := os.ReadFile(filepath.Join(f.parent, "Makefile"))
	if err != nil {
		t.Fatal(err)
	}

	if string(body) != text {
		t.Errorf("the file was touched:\n%s", body)
	}
}
