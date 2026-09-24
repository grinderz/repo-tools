package run

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grinderz/repo-tools/internal/config"
)

func testCtx(names ...string) *Ctx {
	cfg := &config.Config{Confirm: config.ConfirmDestructive}
	for _, n := range names {
		cfg.Projects = append(cfg.Projects, &config.Project{Name: n})
	}

	return &Ctx{Cfg: cfg}
}

func namesOf(ps []*config.Project) string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.Name)
	}

	return strings.Join(out, ",")
}

func TestSelectDefaultsToAllInConfigOrder(t *testing.T) {
	t.Parallel()

	c := testCtx("pylib", "golib", "api", "worker")

	got, err := c.Select(nil)
	if err != nil {
		t.Fatal(err)
	}

	if want := "pylib,golib,api,worker"; namesOf(got) != want {
		t.Fatalf("got %s, want %s", namesOf(got), want)
	}
}

func TestSelectKeepsConfigOrderNotArgumentOrder(t *testing.T) {
	t.Parallel()

	c := testCtx("pylib", "golib", "api", "worker")

	got, err := c.Select([]string{"worker", "pylib"})
	if err != nil {
		t.Fatal(err)
	}

	if want := "pylib,worker"; namesOf(got) != want {
		t.Fatalf("got %s, want %s", namesOf(got), want)
	}
}

func TestSelectSkip(t *testing.T) {
	t.Parallel()

	c := testCtx("a", "b", "c")
	c.Skip = []string{"b"}

	got, err := c.Select(nil)
	if err != nil {
		t.Fatal(err)
	}

	if want := "a,c"; namesOf(got) != want {
		t.Fatalf("got %s, want %s", namesOf(got), want)
	}
}

func TestSelectUnknownNameFails(t *testing.T) {
	t.Parallel()

	c := testCtx("a", "b")
	if _, err := c.Select([]string{"nope"}); err == nil {
		t.Fatal("expected an error for an unknown project")
	}

	c.Skip = []string{"nope"}
	if _, err := c.Select(nil); err == nil {
		t.Fatal("expected an error for an unknown skipped project")
	}
}

func TestSelectEmptyResultFails(t *testing.T) {
	t.Parallel()

	c := testCtx("a")

	c.Skip = []string{"a"}
	if _, err := c.Select(nil); err == nil {
		t.Fatal("expected an error when nothing is selected")
	}
}

// runnableStep is a step Gate has a reason to ask about.
func runnableStep(c *Ctx) Step {
	return Step{Project: c.Cfg.Projects[0], Exec: func() error { return nil }}
}

func TestGateDryRunNeverExecutes(t *testing.T) {
	t.Parallel()

	c := testCtx("a")
	c.DryRun = true

	ok, err := c.Gate("release rc", Destructive, []Step{runnableStep(c)})
	if err != nil || ok {
		t.Fatalf("dry-run should not proceed: ok=%v err=%v", ok, err)
	}
}

func TestGateConfirmModes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		confirm     string
		class       Class
		yes         bool
		wantProceed bool // without touching stdin, so "true" means it did not ask
	}{
		{"destructive config, read-only command", config.ConfirmDestructive, ReadOnly, false, true},
		{"destructive config, local mutation", config.ConfirmDestructive, LocalMutate, false, true},
		{"never config, destructive command", config.ConfirmNever, Destructive, false, true},
		{"destructive command with --yes", config.ConfirmDestructive, Destructive, true, true},
		{"always config with --yes", config.ConfirmAlways, LocalMutate, true, true},
	}
	for _, tc := range cases {
		c := testCtx("a")
		c.Cfg.Confirm = tc.confirm
		c.Yes = tc.yes

		ok, err := c.Gate("cmd", tc.class, []Step{runnableStep(c)})
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}

		if ok != tc.wantProceed {
			t.Errorf("%s: proceed = %v, want %v", tc.name, ok, tc.wantProceed)
		}
	}
}

func TestShowDiffDefaultsOnAndFlagWins(t *testing.T) {
	t.Parallel()

	c := testCtx("a")
	if !c.ShowDiff() {
		t.Error("the diff review should be on by default")
	}

	off := false
	c.Cfg.Diff = &off

	if c.ShowDiff() {
		t.Error("diff: false should turn it off")
	}

	on := true
	c.DiffFlag = &on

	if !c.ShowDiff() {
		t.Error("--diff should win over diff: false")
	}

	c.DiffFlag = &off
	c.Cfg.Diff = nil

	if c.ShowDiff() {
		t.Error("--no-diff should win over the default")
	}
}

func TestInteractive(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		confirm string
		yes     bool
		force   bool
		want    bool
	}{
		{"default", config.ConfirmDestructive, false, false, true},
		{"--yes silences prompts", config.ConfirmDestructive, true, false, false},
		{"confirm never silences prompts", config.ConfirmNever, false, false, false},
		{"--confirm forces prompts back on", config.ConfirmNever, false, true, true},
	}
	for _, tc := range cases {
		c := testCtx("a")
		c.Cfg.Confirm = tc.confirm
		c.Yes, c.ForceConfirm = tc.yes, tc.force

		if got := c.Interactive(); got != tc.want {
			t.Errorf("%s: Interactive() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestGateRejectsYesWithConfirm(t *testing.T) {
	t.Parallel()

	c := testCtx("a")

	c.Yes, c.ForceConfirm = true, true
	if _, err := c.Gate("cmd", Destructive, nil); err == nil {
		t.Fatal("expected --yes with --confirm to be rejected")
	}
}

func TestExecuteSkipsPlannedNoops(t *testing.T) {
	t.Parallel()

	c := testCtx("a", "b")
	ran := 0

	steps := []Step{
		{Project: c.Cfg.Projects[0], Skip: true, Exec: func() error { ran++; return nil }},
		{Project: c.Cfg.Projects[1], Exec: func() error { ran++; return nil }},
	}
	if err := Execute(steps, false); err != nil {
		t.Fatal(err)
	}

	if ran != 1 {
		t.Fatalf("ran %d steps, want 1", ran)
	}
}

// Read-only commands do not fetch and mutating ones do; either default gives
// way to the flags.
func TestWantFetch(t *testing.T) {
	t.Parallel()

	c := testCtx("a")

	if !c.WantFetch(true) || c.WantFetch(false) {
		t.Error("without a flag each command keeps its own default")
	}

	on := true
	c.FetchFlag = &on

	if !c.WantFetch(false) {
		t.Error("--fetch must switch a read-only command on")
	}

	off := false
	c.FetchFlag = &off

	if c.WantFetch(true) {
		t.Error("--no-fetch must switch a mutating command off")
	}
}

// Colour is opt-out and process-wide; with it off nothing is wrapped, which is
// what keeps piped output clean.
//
//nolint:paralleltest // flips the process-wide colour switch
func TestColorHelpers(t *testing.T) {
	SetColor(false)
	defer SetColor(false)

	if got := Yellow("WARNING"); got != "WARNING" {
		t.Errorf("colour off should return the text as is: %q", got)
	}

	SetColor(true)

	got := Yellow("WARNING")
	if !strings.HasPrefix(got, "\x1b[33m") || !strings.HasSuffix(got, "\x1b[0m") {
		t.Errorf("colour on should wrap the text: %q", got)
	}

	if Bold("") != "" {
		t.Error("an empty string needs no escape sequences")
	}
}

// The flag wins over the config, the config over the terminal.
func TestUseColorPrecedence(t *testing.T) {
	t.Parallel()

	c := testCtx("a")
	enabled, disabled := true, false

	c.Cfg.Color = &disabled
	if c.UseColor() {
		t.Error("color: false in the config should win over the terminal")
	}

	c.ColorFlag = &enabled
	if !c.UseColor() {
		t.Error("--color should win over the config")
	}

	c.ColorFlag, c.Cfg.Color = nil, nil
	if c.UseColor() != AutoColor() {
		t.Error("with neither set it is up to the terminal")
	}
}

// A failing project stops the batch unless --keep-going says otherwise.
func TestExecuteStopsAtTheFirstFailure(t *testing.T) {
	t.Parallel()

	c := testCtx("a", "b")
	ran := 0
	steps := []Step{
		{Project: c.Cfg.Projects[0], Exec: func() error { ran++; return errors.New("boom") }},
		{Project: c.Cfg.Projects[1], Exec: func() error { ran++; return nil }},
	}

	if err := Execute(steps, true); err == nil {
		t.Fatal("expected the failure to be reported")
	}

	if ran != 1 {
		t.Errorf("ran %d steps, want 1: the second must not start", ran)
	}

	ran = 0
	if err := Execute(steps, false); err == nil || ran != 2 {
		t.Errorf("--keep-going should run both: ran=%d err=%v", ran, err)
	}
}

// A project may opt out of being fetched — another remote, another hardware
// key — and only an explicit --fetch overrides that.
func TestWantFetchForProject(t *testing.T) {
	t.Parallel()

	c := testCtx("a")
	project := c.Cfg.Projects[0]

	if !c.WantFetchFor(project, true) {
		t.Error("a project that says nothing follows the command's default")
	}

	disabled := false
	project.Fetch = &disabled

	if c.WantFetchFor(project, true) {
		t.Error("fetch: false must stop a fetching command")
	}

	enabled := true
	c.FetchFlag = &enabled

	if !c.WantFetchFor(project, false) {
		t.Error("--fetch is explicit and wins over the project")
	}

	c.FetchFlag = &disabled
	project.Fetch = &enabled

	if c.WantFetchFor(project, true) {
		t.Error("--no-fetch wins too")
	}
}

// Only y/yes, n/no and plain Enter are answers; anything else sends the
// question back — a stray keystroke must not decide a release step.
func TestParseAnswer(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in         string
		byDefault  bool
		want, know bool
	}{
		{"y", false, true, true},
		{"YES", false, true, true},
		{"n", true, false, true},
		{"No", true, false, true},
		{"", false, false, true},
		{"", true, true, true},
		{"q", false, false, false},
		{"da", true, false, false},
	}

	for _, tc := range cases {
		got, ok := parseAnswer(tc.in, tc.byDefault)
		if got != tc.want || ok != tc.know {
			t.Errorf("parseAnswer(%q, %v) = %v, %v", tc.in, tc.byDefault, got, ok)
		}
	}
}

// A plan where every project was skipped has nothing to confirm: asking would
// invite a yes to a run that does nothing. It is not a failure either — the
// plan already says why each project was left alone.
func TestGateWithNothingToDo(t *testing.T) {
	t.Parallel()

	c := testCtx("a")

	ok, err := c.Gate("changelog update", LocalMutate,
		[]Step{{Project: c.Cfg.Projects[0], Skip: true, Warn: "changelog is disabled for this project"}})
	if ok || err != nil {
		t.Fatalf("ok=%v err=%v", ok, err)
	}

	// One runnable project among skipped ones is an ordinary batch.
	if _, err := c.Gate("changelog update", LocalMutate,
		[]Step{{Project: c.Cfg.Projects[0], Skip: true}, runnableStep(c)}); err != nil {
		t.Errorf("a partly skipped plan is fine: %v", err)
	}
}

// A hook sees the event and the run in RT_* variables, a failing hook is a
// warning and not an error, and a dry run fires nothing.
//
//nolint:paralleltest // installs the process-wide hooks and captures stderr
func TestHooksFireWithTheRunInTheEnvironment(t *testing.T) {
	log := filepath.Join(t.TempDir(), "hooks.log")

	SetHooks(map[string][]string{
		EventAsk: {`echo "$RT_EVENT|$RT_COMMAND|$RT_PROJECT|$RT_MESSAGE" >> "` + log + `"`},
	}, "deps submodules api", false)

	defer SetHooks(nil, "", false)

	setProject("api", "/srv/api")
	Fire(EventAsk, map[string]string{EnvMessage: "Commit and push?"})
	setProject("", "")

	got, err := os.ReadFile(log)
	if err != nil || strings.TrimSpace(string(got)) != "ask|deps submodules api|api|Commit and push?" {
		t.Errorf("hook log = %q, %v", got, err)
	}

	// An event without hooks, and a dry run, run nothing.
	Fire(EventDone, nil)

	SetHooks(map[string][]string{EventAsk: {"echo dry >> " + log}}, "x", true)
	Fire(EventAsk, nil)

	if got, _ = os.ReadFile(log); strings.Contains(string(got), "dry") || strings.Count(string(got), "\n") != 1 {
		t.Errorf("only the one hook should have run: %q", got)
	}

	// A broken hook is reported and swallowed.
	SetHooks(map[string][]string{EventDone: {"exit 3"}}, "x", false)

	stderr := os.Stderr

	pipeR, pipeW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}

	os.Stderr = pipeW

	Fire(EventDone, nil)

	os.Stderr = stderr

	_ = pipeW.Close()

	warning := make([]byte, 256)
	n, _ := pipeR.Read(warning)

	if !strings.Contains(string(warning[:n]), "WARNING: hook done: exit 3: hook: exit status 3") {
		t.Errorf("the failure should be a warning: %q", warning[:n])
	}
}

// The question reaches the hook as plain text: no colour codes, no prompt
// punctuation.
func TestPlainTextStripsThePrompt(t *testing.T) {
	t.Parallel()

	if got := plainText("\x1b[1mProceed?\x1b[0m\x1b[2m [y/N]\x1b[0m: "); got != "Proceed? [y/N]" {
		t.Errorf("got %q", got)
	}
}
