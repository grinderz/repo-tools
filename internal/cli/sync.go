package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/grinderz/repo-tools/internal/config"
	"github.com/grinderz/repo-tools/internal/gitx"
	"github.com/grinderz/repo-tools/internal/run"
)

// syncOptions is what the flags of repo sync ask for.
type syncOptions struct {
	NoPull   bool // fetch only, leave the dev branch where it is
	Checkout bool // switch the working copy to the dev branch as well
}

func newSyncCmd(rctx *run.Ctx, name string) *cobra.Command {
	var opts syncOptions

	c := &cobra.Command{
		Use:   name + " [project...]",
		Short: "Clone missing projects, fetch origin and fast-forward the dev branch",
		Long: "Clones the projects that are missing, fetches origin and fast-forwards\n" +
			"the dev branch — as a ref move when another branch is checked out, so\n" +
			"the working copy stays where it is. --checkout switches it to the dev\n" +
			"branch too, with the submodules on that branch's pins; a project with\n" +
			"uncommitted changes is shown in the plan and left alone, since a\n" +
			"checkout there would carry the changes across or refuse half way.",
		RunE: func(cmd *cobra.Command, args []string) error {
			projects, err := rctx.Select(args)
			if err != nil {
				return err
			}

			steps := make([]run.Step, 0, len(projects))
			for _, p := range projects {
				steps = append(steps, planSync(rctx, p, opts))
			}

			ok, err := rctx.Gate(cmdLabel(cmd), run.LocalMutate, steps)
			if err != nil || !ok {
				return err
			}

			return run.Execute(steps, !rctx.KeepGoing)
		},
	}
	c.Flags().BoolVar(&opts.NoPull, "no-pull", false, "fetch only, do not fast-forward the dev branch")
	c.Flags().BoolVar(&opts.Checkout, "checkout", false,
		"switch the working copy to the dev branch as well (clean trees only)")

	return c
}

func planSync(rctx *run.Ctx, p *config.Project, opts syncOptions) run.Step {
	r := repoOf(p)
	st := run.Step{Project: p}

	switch {
	case !r.Exists():
		st.Plan = []string{fmt.Sprintf("clone %s -> %s (branch %s)", p.Git, r.Dir, planRef(p.DevBranch))}
		st.Exec = func() error { return gitx.Clone(p.Git, p.Dir(), p.DevBranch) }
	case !r.IsRepo():
		st.Skip = true
		st.Warn = r.Dir + " exists but is not a git repository"
	default:
		if rctx.WantFetchFor(p, true) {
			st.Plan = []string{"fetch --prune --tags origin"}
		} else {
			st.Plan = []string{"no fetch, working with the refs already here"}
		}

		if opts.Checkout && !planCheckout(&st, r, p) {
			return st
		}

		if !opts.NoPull {
			st.Plan = append(st.Plan, "fast-forward "+planRef(p.DevBranch)+" to origin")
		}

		st.Exec = func() error { return syncProject(rctx, p, opts) }
	}

	return st
}

// planCheckout adds the branch switch to the plan and says whether the step
// still runs. A dirty tree is the one thing the switch must not meet: git
// would either carry the changes across or stop half way, so the project is
// skipped with the changes named — the look before the leap, per project.
func planCheckout(st *run.Step, r gitx.Repo, p *config.Project) bool {
	changed, err := dirtyFiles(r, true)
	if err != nil {
		st.Skip, st.Warn = true, err.Error()

		return false
	}

	if len(changed) > 0 {
		st.Skip = true
		st.Warn = fmt.Sprintf("working tree is dirty: %s; commit or run repo clean first", summarizeFiles(changed))

		return false
	}

	current, err := r.CurrentBranch()
	if err != nil {
		st.Skip, st.Warn = true, err.Error()

		return false
	}

	if current == p.DevBranch {
		return true
	}

	st.Plan = append(st.Plan, fmt.Sprintf("checkout %s (from %s), submodules to its pins",
		planRef(p.DevBranch), planRef(current)))

	// Nothing is lost by leaving a branch behind, but work that only exists
	// here is easy to forget once it is out of sight.
	if ahead, _, err := r.AheadBehind("origin/" + current); err == nil && ahead > 0 {
		st.Warn = fmt.Sprintf("leaves %s with %d commit(s) that are not on origin", current, ahead)
	}

	return true
}

// updateBranchRef fast-forwards a branch that is not checked out by moving its
// ref to the one on origin. It is a local operation, so --no-fetch stays
// honest, and anything that is not a fast-forward is refused rather than forced.
func updateBranchRef(r gitx.Repo, branch string) error {
	origin := "refs/remotes/origin/" + branch

	if !r.LocalBranchExists(branch) {
		_, err := r.Git("branch", branch, origin)

		return err
	}

	if _, err := r.Git("merge-base", "--is-ancestor", branch, origin); err != nil {
		return fmt.Errorf("%s has diverged from origin, fast-forward it by hand", branch)
	}

	_, err := r.Git("update-ref", "refs/heads/"+branch, origin)

	return err
}

func syncProject(rctx *run.Ctx, p *config.Project, opts syncOptions) error {
	r := repoOf(p)
	if err := fetch(rctx, p, r); err != nil {
		return err
	}

	if opts.NoPull && !opts.Checkout {
		return nil
	}

	if !r.RemoteBranchExists(p.DevBranch) {
		return fmt.Errorf("origin/%s does not exist", p.DevBranch)
	}

	clean, err := r.IsClean()
	if err != nil {
		return err
	}

	if !clean {
		return errors.New("working tree is dirty, skipping fast-forward")
	}

	current, err := r.CurrentBranch()
	if err != nil {
		return err
	}

	if opts.Checkout && current != p.DevBranch {
		if err := checkoutDev(r, p.DevBranch); err != nil {
			return err
		}

		current = p.DevBranch
	}

	if opts.NoPull {
		return nil
	}

	if current != p.DevBranch {
		return updateBranchRef(r, p.DevBranch)
	}

	_, err = r.Git("merge", "--ff-only", "origin/"+p.DevBranch)

	return err
}

// checkoutDev switches a clean working copy to the dev branch, creating the
// local branch from origin when it is not there yet, and puts the submodules
// on the pins that branch records — a switch that left them on the old
// branch's pins would look dirty a moment later.
func checkoutDev(r gitx.Repo, branch string) error {
	if r.LocalBranchExists(branch) {
		if _, err := r.Git("checkout", "--quiet", branch); err != nil {
			return err
		}
	} else if _, err := r.Git("checkout", "--quiet", "-b", branch, "--track", "origin/"+branch); err != nil {
		return err
	}

	if _, err := r.Git("submodule", "update", "--init", "--recursive"); err != nil {
		return err
	}

	fmt.Printf("    checked out %s\n", planRef(branch))

	return nil
}
