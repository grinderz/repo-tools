package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/grinderz/repo-tools/internal/config"
	"github.com/grinderz/repo-tools/internal/gitx"
	"github.com/grinderz/repo-tools/internal/run"
)

func newSyncCmd(rctx *run.Ctx, name string) *cobra.Command {
	var noPull bool

	c := &cobra.Command{
		Use:   name + " [project...]",
		Short: "Clone missing projects, fetch origin and fast-forward the dev branch",
		RunE: func(cmd *cobra.Command, args []string) error {
			projects, err := rctx.Select(args)
			if err != nil {
				return err
			}

			steps := make([]run.Step, 0, len(projects))
			for _, p := range projects {
				steps = append(steps, planSync(rctx, p, noPull))
			}

			ok, err := rctx.Gate(cmdLabel(cmd), run.LocalMutate, steps)
			if err != nil || !ok {
				return err
			}

			return run.Execute(steps, !rctx.KeepGoing)
		},
	}
	c.Flags().BoolVar(&noPull, "no-pull", false, "fetch only, do not fast-forward the dev branch")

	return c
}

func planSync(rctx *run.Ctx, p *config.Project, noPull bool) run.Step {
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

		if !noPull {
			st.Plan = append(st.Plan, "fast-forward "+planRef(p.DevBranch)+" to origin")
		}

		st.Exec = func() error { return syncProject(rctx, p, noPull) }
	}

	return st
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

func syncProject(rctx *run.Ctx, p *config.Project, noPull bool) error {
	r := repoOf(p)
	if err := fetch(rctx, p, r); err != nil {
		return err
	}

	if noPull {
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

	if current != p.DevBranch {
		return updateBranchRef(r, p.DevBranch)
	}

	_, err = r.Git("merge", "--ff-only", "origin/"+p.DevBranch)

	return err
}
