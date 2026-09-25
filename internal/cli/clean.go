package cli

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/grinderz/repo-tools/internal/config"
	"github.com/grinderz/repo-tools/internal/gitx"
	"github.com/grinderz/repo-tools/internal/run"
)

// dirtyPreviewLines caps the file list a plan line carries; the review that
// follows shows all of it.
const dirtyPreviewLines = 3

func newCleanCmd(rctx *run.Ctx, name string) *cobra.Command {
	var untracked bool

	c := &cobra.Command{
		Use:   name + " [project...]",
		Short: "Discard uncommitted changes and restore submodule pins",
		Long: "Resets the working tree to HEAD and puts every submodule back on the\n" +
			"pin the branch records. This is what clears the leftovers of a run that\n" +
			"stopped half way, which every committing command refuses to start on.\n\n" +
			"Tracked files only, unless --untracked also removes what git does not\n" +
			"know about. Uncommitted work is not recoverable afterwards, so the\n" +
			"changes are shown and confirmed first.",
		RunE: func(cmd *cobra.Command, args []string) error {
			projects, err := rctx.Select(args)
			if err != nil {
				return err
			}

			steps := make([]run.Step, 0, len(projects))
			for _, p := range projects {
				steps = append(steps, planClean(rctx, p, untracked))
			}

			ok, err := rctx.Gate(cmdLabel(cmd), run.Destructive, steps)
			if err != nil || !ok {
				return err
			}

			return run.Execute(steps, !rctx.KeepGoing)
		},
	}
	c.Flags().BoolVar(&untracked, "untracked", false, "also delete files git does not track")

	return c
}

func planClean(rctx *run.Ctx, p *config.Project, untracked bool) run.Step {
	r := repoOf(p)
	st := run.Step{Project: p}

	if reason := missingRepo(r); reason != "" {
		st.Skip, st.Warn = true, reason

		return st
	}

	changed, err := dirtyFiles(r, untracked)
	if err != nil {
		st.Skip, st.Warn = true, err.Error()

		return st
	}

	if len(changed) == 0 {
		st.Skip, st.Warn = true, "working tree is already clean"

		return st
	}

	st.Plan = append(st.Plan, fmt.Sprintf("discard %d change(s): %s", len(changed), summarizeFiles(changed)))
	st.Plan = append(st.Plan, "restore submodules to the recorded pins")

	if untracked {
		st.Plan = append(st.Plan, "delete untracked files too")
	}

	if rctx.ShowDiff() {
		st.Plan = append(st.Plan, "(changes shown first)")
	}

	st.Exec = func() error { return cleanProject(rctx, p, untracked) }

	return st
}

// dirtyFiles lists the porcelain entries a clean would throw away, the ones
// inside submodules included, as "<submodule>/<file>". The parent's status
// shows a submodule as one modified path whether its pin moved or a file in
// it was edited, and the edits are what a checkout of the pin would refuse
// to overwrite — so they have to be on the list that is shown and confirmed.
// Untracked files are only counted when they are going to be deleted.
func dirtyFiles(r gitx.Repo, untracked bool) ([]string, error) {
	lines, err := r.Lines(statusArgs(untracked)...)
	if err != nil {
		return nil, err
	}

	paths, err := r.SubmodulePaths()
	if err != nil {
		return lines, nil //nolint:nilerr // no .gitmodules to speak of, nothing to descend into
	}

	for _, path := range paths {
		sub := gitx.Repo{Dir: filepath.Join(r.Dir, path)}
		if !sub.IsRepoRoot() {
			continue
		}

		inner, err := dirtyFiles(sub, untracked)
		if err != nil {
			return nil, fmt.Errorf("submodule %s: %w", path, err)
		}

		for _, line := range inner {
			status, file, _ := strings.Cut(line, " ")
			lines = append(lines, status+" "+path+"/"+file)
		}
	}

	return lines, nil
}

func statusArgs(untracked bool) []string {
	args := []string{"status", "--porcelain"}
	if !untracked {
		args = append(args, "--untracked-files=no")
	}

	return args
}

// dirtySubmodules names the submodules with changes of their own, for the
// review to show each one's diff under its path.
func dirtySubmodules(r gitx.Repo, untracked bool) []string {
	paths, err := r.SubmodulePaths()
	if err != nil {
		return nil
	}

	var dirty []string

	for _, path := range paths {
		sub := gitx.Repo{Dir: filepath.Join(r.Dir, path)}
		if !sub.IsRepoRoot() {
			continue
		}

		if inner, err := sub.Lines(statusArgs(untracked)...); err == nil && len(inner) > 0 {
			dirty = append(dirty, path)
		}
	}

	return dirty
}

// summarizeFiles keeps a plan line to one line.
func summarizeFiles(changed []string) string {
	names := make([]string, 0, len(changed))
	for _, line := range changed {
		names = append(names, strings.TrimSpace(line))
	}

	if len(names) <= dirtyPreviewLines {
		return strings.Join(names, ", ")
	}

	return fmt.Sprintf("%s and %d more", strings.Join(names[:dirtyPreviewLines], ", "),
		len(names)-dirtyPreviewLines)
}

func cleanProject(rctx *run.Ctx, p *config.Project, untracked bool) error {
	r := repoOf(p)

	ok, err := reviewDiscard(rctx, r, p.Name, untracked)
	if err != nil {
		return err
	}

	if !ok {
		fmt.Printf("    %s, %s was left as it is\n", run.Yellow("declined"), r.Dir)

		return errDeclined
	}

	if _, err := r.Git("reset", "--hard"); err != nil {
		return err
	}

	// --force: a submodule's own edits go the way the parent's did, since
	// they were on the list that was just confirmed; without it the checkout
	// of the pin refuses to overwrite them and the clean stops half way.
	if _, err := r.Git("submodule", "update", "--init", "--recursive", "--force"); err != nil {
		return err
	}

	if untracked {
		if _, err := r.Git("clean", "-fd"); err != nil {
			return err
		}

		if _, err := r.Git("submodule", "foreach", "--recursive", "git", "clean", "-fd"); err != nil {
			return err
		}
	}

	fmt.Println("    " + run.Green("clean"))

	return nil
}

// reviewDiscard shows what is about to be thrown away. Nothing here can be
// recovered afterwards, which is why this asks even though the plan already
// counted the files.
func reviewDiscard(rctx *run.Ctx, r gitx.Repo, label string, untracked bool) (bool, error) {
	if !rctx.ShowDiff() {
		return true, nil
	}

	changed, err := dirtyFiles(r, untracked)
	if err != nil {
		return false, err
	}

	fmt.Printf("\n%s is about to discard:\n%s\n", run.Bold(label), strings.Join(changed, "\n"))

	diff, err := r.Git("diff", "HEAD", colorArg(), submoduleLog)
	if err != nil {
		return false, err
	}

	printCapped(rctx, diff, "git -C "+r.Dir+" diff HEAD "+submoduleLog)

	// A submodule's own edits are invisible in the parent's diff, which only
	// says the submodule changed; each one gets its diff under its path.
	for _, path := range dirtySubmodules(r, untracked) {
		sub := gitx.Repo{Dir: filepath.Join(r.Dir, path)}

		inner, err := sub.Git("diff", "HEAD", colorArg())
		if err != nil {
			return false, fmt.Errorf("submodule %s: %w", path, err)
		}

		fmt.Printf("\ninside %s:\n", run.Bold(path))
		printCapped(rctx, inner, "git -C "+sub.Dir+" diff HEAD")
	}

	if !rctx.Interactive() {
		return true, nil
	}

	return run.Confirm("Discard these changes?")
}
