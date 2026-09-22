// Package run holds batch mechanics: project selection, plan display,
// confirmation gating and sequential execution in config order.
package run

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/grinderz/repo-tools/internal/config"
)

// Class of a command, drives confirmation.
type Class int

const (
	ReadOnly    Class = iota // never asks
	LocalMutate              // asks only with confirm: always
	Destructive              // asks with confirm: destructive|always
)

type Ctx struct {
	Cfg          *config.Config
	DryRun       bool
	Yes          bool
	ForceConfirm bool
	// DiffFlag overrides the config's diff when --diff or --no-diff was given.
	DiffFlag *bool
	// FetchFlag overrides a command's own fetching default when --fetch or
	// --no-fetch was given.
	FetchFlag *bool
	// ColorFlag overrides the config and the terminal detection when --color
	// or --no-color was given.
	ColorFlag *bool
	// KeepGoing lets a batch continue after a project fails; by default the
	// first failure stops the run, because the projects after it usually
	// depend on the one that broke.
	KeepGoing bool
	Skip      []string
	// NoCI skips the pipeline watch after pushes for one run, whatever the
	// projects' ci settings say.
	NoCI bool
	// Aborted records that the operator declined the last plan's question.
	// A declined command exits cleanly on its own, but a flow reads this to
	// stop instead of carrying on to the next command as if nothing happened.
	Aborted bool
	// ConfigPath is where Cfg was loaded from, for messages that tell the
	// operator which file said so.
	ConfigPath string
}

// UseColor decides colouring: the flag, then the config, then whether a
// terminal is attached.
func (c *Ctx) UseColor() bool {
	if c.ColorFlag != nil {
		return *c.ColorFlag
	}

	if c.Cfg != nil && c.Cfg.Color != nil {
		return *c.Cfg.Color
	}

	return AutoColor()
}

// ShowDiff reports whether a commit shows its diff first: the flag when it was
// given, the config otherwise.
func (c *Ctx) ShowDiff() bool {
	if c.DiffFlag != nil {
		return *c.DiffFlag
	}

	return c.Cfg.DiffEnabled()
}

// WantFetch reports whether to refresh remote refs. Commands that act on the
// remote fetch by default; the read-only ones do not, and either default gives
// way to --fetch or --no-fetch.
func (c *Ctx) WantFetch(byDefault bool) bool {
	if c.FetchFlag != nil {
		return *c.FetchFlag
	}

	return byDefault
}

// WantFetchFor is WantFetch for one project: a project may opt out of being
// fetched at all, which an explicit --fetch still overrides.
func (c *Ctx) WantFetchFor(p *config.Project, byDefault bool) bool {
	if c.FetchFlag != nil {
		return *c.FetchFlag
	}

	if p != nil && !p.FetchAllowed() {
		return false
	}

	return byDefault
}

// Interactive reports whether the operator is available to answer questions.
// Nothing prompts under --yes or confirm: never.
func (c *Ctx) Interactive() bool {
	if c.ForceConfirm {
		return true
	}

	return !c.Yes && c.Cfg.Confirm != config.ConfirmNever
}

// stdin is shared: a fresh bufio.Reader per prompt would swallow whatever it
// read ahead, so a second question in the same run would see EOF.
var stdin = bufio.NewReader(os.Stdin) //nolint:gochecknoglobals // one terminal per process

// flushTypeahead drops keystrokes typed while no question was on screen. A
// long diff or a pipeline watch collects them, and the buffered Enter would
// then answer the next question by itself — for a destructive one that means
// declining a release step nobody looked at. An answer is deliberate only
// after the question is visible. A pipe is left alone: its answers are fed
// ahead on purpose.
func flushTypeahead() {
	fi, err := os.Stdin.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return
	}

	if n := stdin.Buffered(); n > 0 {
		_, _ = stdin.Discard(n)
	}

	flushTTYInput(os.Stdin)
}

// Prompt asks for a line of input and returns it trimmed.
func Prompt(question string) (string, error) {
	flushTypeahead()

	// A blank line and a mark of its own: a question that follows a diff or a
	// plan has to be findable at a glance, and answerable without scrolling
	// back to see what it is about.
	fmt.Printf("\n%s %s", Marker(), question)

	line, err := stdin.ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("read answer: %w", err)
	}

	// The same air below: whatever runs next starts its own block instead of
	// hanging off the answered question.
	fmt.Println()

	return strings.TrimSpace(line), nil
}

// Confirm asks a yes/no question, defaulting to no.
func Confirm(question string) (bool, error) { return confirm(question, false) }

// ConfirmYes asks a yes/no question defaulting to yes — for "try again?",
// where plain Enter is the expected answer.
func ConfirmYes(question string) (bool, error) { return confirm(question, true) }

// confirm keeps asking until it gets an answer: y/yes, n/no, or plain Enter
// for the default. Anything else is not an answer at all — a stray keystroke
// must not decide a release step either way.
func confirm(question string, byDefault bool) (bool, error) {
	hint := Dim(" [y/N]")
	if byDefault {
		hint = Dim(" [Y/n]")
	}

	for {
		answer, err := Prompt(Bold(question) + hint + ": ")
		if err != nil {
			return false, err
		}

		if value, ok := parseAnswer(answer, byDefault); ok {
			return value, nil
		}

		fmt.Println(Dim("answer y or n (Enter for the default)"))
	}
}

// parseAnswer maps an answer onto yes/no; the second result is false when the
// input is neither, and the question has to be asked again.
func parseAnswer(answer string, byDefault bool) (bool, bool) {
	switch strings.ToLower(answer) {
	case "":
		return byDefault, true
	case "y", "yes":
		return true, true
	case "n", "no":
		return false, true
	}

	return false, false
}

// Select filters config projects by names (all if empty), preserving config
// order regardless of argument order. Unknown names fail before execution.
func (c *Ctx) Select(names []string) ([]*config.Project, error) {
	byName := map[string]*config.Project{}
	for _, p := range c.Cfg.Projects {
		byName[p.Name] = p
	}

	for _, n := range append(append([]string{}, names...), c.Skip...) {
		if _, ok := byName[n]; !ok {
			return nil, fmt.Errorf("project %q %w", n, errUnknownProject)
		}
	}

	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}

	skip := map[string]bool{}
	for _, n := range c.Skip {
		skip[n] = true
	}

	var out []*config.Project

	for _, p := range c.Cfg.Projects {
		if len(want) > 0 && !want[p.Name] {
			continue
		}

		if skip[p.Name] {
			continue
		}

		out = append(out, p)
	}

	if len(out) == 0 {
		return nil, errNoProjects
	}

	return out, nil
}

// Step is one project's part of a batch command.
type Step struct {
	Project *config.Project
	Plan    []string     // human-readable plan lines
	Warn    string       // shown in plan; step still runs unless Skip
	Skip    bool         // planned as no-op (with Warn as the reason)
	Exec    func() error // nil for read-only/skipped steps
}

// Gate prints the plan and applies dry-run/confirmation rules.
// Returns true when execution should proceed.
func (c *Ctx) Gate(name string, class Class, steps []Step) (bool, error) {
	c.Aborted = false

	fmt.Printf("%s %s\n", Marker(), Bold(name+" plan"))

	for _, s := range steps {
		status := ""
		if s.Skip {
			status = Dim("  [skip]")
		}

		fmt.Printf("  %s%s\n", Bold(s.Project.Name), status)

		for _, l := range s.Plan {
			fmt.Printf("      %s\n", l)
		}

		if s.Warn != "" {
			fmt.Printf("      %s %s\n", Warn(), s.Warn)
		}
	}

	if c.DryRun {
		fmt.Println("[dry-run] nothing executed")
		return false, nil
	}

	if c.Yes && c.ForceConfirm {
		return false, errYesAndConfirm
	}

	// Every project was skipped, so there is nothing to confirm: asking would
	// invite a yes to a run that then does nothing. Not an error — the plan
	// above already says why each project was left alone.
	if !runnable(steps) {
		fmt.Println(Dim("nothing to do"))
		return false, nil
	}

	ask := false

	switch class {
	case ReadOnly:
		ask = false
	case LocalMutate:
		ask = c.Cfg.Confirm == config.ConfirmAlways
	case Destructive:
		ask = c.Cfg.Confirm != config.ConfirmNever
	}

	if c.Yes {
		ask = false
	}

	if c.ForceConfirm {
		ask = true
	}

	if !ask {
		return true, nil
	}

	proceed, err := Confirm("Proceed?")
	if err != nil {
		return false, err
	}

	if !proceed {
		c.Aborted = true

		fmt.Println(Dim("aborted"))
	}

	return proceed, nil
}

// runnable reports whether any step would actually do something, by the same
// rule Execute applies.
func runnable(steps []Step) bool {
	for _, s := range steps {
		if !s.Skip && s.Exec != nil {
			return true
		}
	}

	return false
}

// Execute runs steps sequentially in the given (config) order.
// A failed step is reported and the batch continues, unless stopOnError.
func Execute(steps []Step, stopOnError bool) error {
	var failed []string

	for _, s := range steps {
		if s.Skip || s.Exec == nil {
			continue
		}

		fmt.Printf("%s %s\n", Arrow(), Bold(s.Project.Name))

		if err := s.Exec(); err != nil {
			// The reason is printed here, in the project's own context; what
			// travels up is only which project stopped the run, or the caller
			// would print the whole thing a second time on the way out.
			fmt.Fprintf(os.Stderr, "%s %s: %v\n", Fail(), s.Project.Name, err)

			failed = append(failed, s.Project.Name)
			if stopOnError {
				return fmt.Errorf("%w %s, see the error above", errStopped, s.Project.Name)
			}
		}
	}

	if len(failed) > 0 {
		return fmt.Errorf("%w: %s", errFailedProjects, strings.Join(failed, ", "))
	}

	return nil
}
