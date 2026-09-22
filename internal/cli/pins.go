package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/grinderz/repo-tools/internal/config"
	"github.com/grinderz/repo-tools/internal/gitx"
	"github.com/grinderz/repo-tools/internal/run"
)

// Not every internal dependency is a submodule. A go project consumes its
// libraries through go.mod, and the refs those are resolved from live in a make
// variable — GO_DEPS_UPDATE_INTERNAL, which make deps.update.internal walks.
// Freezing a release branch has to move them as well: leaving them on develop
// would have the deps commands write dev-head pseudo-versions into go.mod, the
// exact opposite of a freeze.

// modulePin matches one module@ref of such a list. The ref stops at whitespace
// and at the backslash that continues a make variable onto the next line.
var modulePin = regexp.MustCompile(`([^\s@\\]+)@([^\s\\]+)`)

// pinChange is one ref the freeze moves.
type pinChange struct {
	Module string
	From   string
	To     string
}

func (c pinChange) String() string {
	return fmt.Sprintf("%s@%s -> %s", c.Module, c.From, planRef(c.To))
}

// planPins describes what freezing the module refs on ref would do. It reads
// the branch being frozen, not the checkout, for the same reason the submodule
// plan does: the two can name different dependencies.
func planPins(rctx *run.Ctx, r gitx.Repo, p *config.Project, ref string) ([]string, error) {
	pin, ok := rctx.Cfg.DepsPinsFor(p)
	if !ok {
		return nil, nil
	}

	text, found, err := r.FileAt(ref, pin.File)

	switch {
	case err != nil:
		return nil, err
	case !found:
		return []string{fmt.Sprintf("%s MISSING on %s, module pins not frozen", pin.File, refName(ref))}, nil
	}

	_, changes, ok, err := rewritePins(rctx, text, pin.Var)
	if err != nil {
		return nil, fmt.Errorf("%s on %s: %w", pin.File, refName(ref), err)
	}

	if !ok {
		return []string{fmt.Sprintf("%s: no %s, module pins not frozen", pin.File, pin.Var)}, nil
	}

	lines := make([]string, 0, len(changes))
	for _, c := range changes {
		lines = append(lines, fmt.Sprintf("%s %s: %s", pin.File, pin.Var, c))
	}

	return lines, nil
}

// freezePins moves the refs in the checked-out file and reports what it moved.
func freezePins(rctx *run.Ctx, r gitx.Repo, p *config.Project) error {
	pin, ok := rctx.Cfg.DepsPinsFor(p)
	if !ok {
		return nil
	}

	path := filepath.Join(r.Dir, pin.File)

	text, found, err := r.FileAt("", pin.File)
	if err != nil {
		return err
	}

	if !found {
		fmt.Printf("    %s %s is not here, module pins not frozen\n", run.Warn(), pin.File)

		return nil
	}

	updated, changes, ok, err := rewritePins(rctx, text, pin.Var)
	if err != nil {
		return fmt.Errorf("%s: %w", pin.File, err)
	}

	if !ok {
		fmt.Printf("    %s %s has no %s, module pins not frozen\n", run.Warn(), pin.File, pin.Var)

		return nil
	}

	if len(changes) == 0 {
		return nil
	}

	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat %s: %w", pin.File, err)
	}

	if err := os.WriteFile(path, []byte(updated), info.Mode().Perm()); err != nil {
		return fmt.Errorf("write %s: %w", pin.File, err)
	}

	for _, c := range changes {
		fmt.Printf("    %s %s\n", pin.File, c)
	}

	return nil
}

// rewritePins moves every module@ref of the named variable onto the branch the
// module's own project works on in this config — the same rule
// freeze_to: project follows, so a dev config pins dev branches and a release
// config pins release branches. The third result is false when the file has no
// such variable at all.
//
// A module no project in this config provides is an error, not something to
// leave alone: the variable lists internal dependencies, so an unknown one
// means the config is missing a project, and freezing the rest would ship a
// release with one dependency still following its dev branch.
func rewritePins(rctx *run.Ctx, text, name string) (string, []pinChange, bool, error) {
	lines := strings.Split(text, "\n")

	first, last, ok := variableLines(lines, name)
	if !ok {
		return text, nil, false, nil
	}

	var (
		changes []pinChange
		unknown []string
	)

	for i := first; i <= last; i++ {
		lines[i] = modulePin.ReplaceAllStringFunc(lines[i], func(match string) string {
			module, ref, _ := strings.Cut(match, "@")

			provider := rctx.Cfg.ProjectByGit(module)
			if provider == nil {
				unknown = append(unknown, module)

				return match
			}

			target := provider.TargetBranch()
			if target == ref {
				return match
			}

			changes = append(changes, pinChange{Module: module, From: ref, To: target})

			return module + "@" + target
		})
	}

	if len(unknown) > 0 {
		return text, nil, true, fmt.Errorf(
			"%s lists %s, which %s %w",
			name, strings.Join(unknown, ", "), plural(len(unknown), "is", "are"), errNotAProject)
	}

	return strings.Join(lines, "\n"), changes, true, nil
}

// plural picks the verb form for a list that is usually one item long.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}

	return many
}

// variableLines is the span of a make variable's value: the assignment line and
// every line a trailing backslash continues it onto.
func variableLines(lines []string, name string) (int, int, bool) {
	assignment := regexp.MustCompile(`^\s*` + regexp.QuoteMeta(name) + `\s*[:?+]?=`)

	for first, line := range lines {
		if !assignment.MatchString(line) {
			continue
		}

		last := first
		for last < len(lines)-1 && strings.HasSuffix(strings.TrimSpace(lines[last]), `\`) {
			last++
		}

		return first, last, true
	}

	return 0, 0, false
}
