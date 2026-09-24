package cli

import (
	"fmt"
	"slices"

	"github.com/spf13/cobra"

	"github.com/grinderz/repo-tools/internal/config"
	"github.com/grinderz/repo-tools/internal/gitx"
	"github.com/grinderz/repo-tools/internal/run"
)

// submoduleTarget is one submodule to move, with the branch .gitmodules says
// it tracks.
type submoduleTarget struct {
	Path   string
	Branch string
}

// submodulesOpts are the switches of deps submodules, passed around as one
// value so the plan and the execution cannot drift apart.
type submodulesOpts struct {
	Branch     string
	Only       []string
	NoCommit   bool
	NoDeps     bool
	AllowDirty bool
}

func newSubmodulesCmd(rctx *run.Ctx, name string) *cobra.Command {
	var opts submodulesOpts

	c := &cobra.Command{
		Use:   name + " [project...]",
		Short: "Update submodules to the head of the branch they track and push",
		Long: "Runs git submodule update --remote in every selected project, moving\n" +
			"each submodule to the head of the branch .gitmodules has it tracking,\n" +
			"runs the project's deps commands, then commits and pushes. Unlike deps\n" +
			"freeze it rewrites no branch names in .gitmodules: whatever a submodule\n" +
			"already tracks is what it is moved along.\n\n" +
			"Works on the dev branch unless --branch says otherwise. A submodule\n" +
			"without a branch in .gitmodules is skipped, since --remote would\n" +
			"otherwise follow the remote's default branch.",
		RunE: func(cmd *cobra.Command, args []string) error {
			projects, err := rctx.Select(args)
			if err != nil {
				return err
			}

			steps := make([]run.Step, 0, len(projects))
			matched := 0

			for _, p := range projects {
				step, found, err := planSubmodules(rctx, p, opts)
				if err != nil {
					return fmt.Errorf("%s: %w", p.Name, err)
				}

				steps = append(steps, step)
				matched += found
			}

			if len(opts.Only) > 0 && matched == 0 {
				return fmt.Errorf("%w %v", errNoSuchSubs, opts.Only)
			}

			class := run.Destructive
			if opts.NoCommit {
				class = run.LocalMutate
			}

			ok, err := rctx.Gate(cmdLabel(cmd), class, steps)
			if err != nil || !ok {
				return err
			}

			return run.Execute(steps, !rctx.KeepGoing)
		},
	}
	c.Flags().StringVar(
		&opts.Branch,
		varBranch,
		"",
		"branch to update the submodules on (default: dev_branch)",
	)
	c.Flags().StringSliceVar(
		&opts.Only,
		"submodule",
		nil,
		"only these submodule paths (comma separated or repeated)",
	)
	c.Flags().BoolVar(
		&opts.NoCommit,
		"no-commit",
		false,
		"leave the new pins in the working tree instead of committing and pushing",
	)
	c.Flags().BoolVar(&opts.NoDeps, "no-deps", false, "skip the deps commands, only move the submodule pins")
	c.Flags().BoolVar(&opts.AllowDirty, "allow-dirty", false,
		"start on a working tree that already has changes, and commit them along (needs the diff review)")

	return c
}

func planSubmodules(rctx *run.Ctx, p *config.Project, opts submodulesOpts) (run.Step, int, error) {
	r := repoOf(p)
	st := run.Step{Project: p}

	if reason := missingRepo(r); reason != "" {
		st.Skip, st.Warn = true, reason

		return st, 0, nil
	}

	if err := fetch(rctx, p, r); err != nil {
		return st, 0, fetchFailed(err)
	}

	branch := opts.Branch
	if branch == "" {
		branch = p.DevBranch
	}

	targets, untracked, err := configuredSubmodules(rctx, r, p, branchRef(r, branch), opts.Only)
	if err != nil {
		st.Skip, st.Warn = true, err.Error()

		return st, 0, nil //nolint:nilerr // the error becomes the skip reason, not a failure
	}

	if len(targets) == 0 {
		st.Skip, st.Warn = true, noSubmodulesReason(opts.Only, untracked)

		return st, 0, nil
	}

	st.Plan = append(st.Plan, "checkout "+planRef(branch))
	for _, t := range targets {
		st.Plan = append(st.Plan, fmt.Sprintf("submodule %s -> head of %s, update --remote",
			t.Path, planRef(t.Branch)))
	}

	if len(untracked) > 0 {
		st.Warn = fmt.Sprintf("no branch in .gitmodules, left alone: %v", untracked)
	}

	if !opts.NoDeps {
		st.Plan = append(st.Plan, direnvPlanLines(rctx, r)...)
		st.Plan = append(st.Plan, planDeps(rctx, p, branch)...)
	}

	if opts.NoCommit {
		st.Plan = append(st.Plan, "leave the new pins uncommitted")
	} else {
		st.Plan = append(st.Plan,
			commitPlanLines(rctx, p, cmdDepsSubmodules, branch, "any pin moved", submodulesMessage(rctx, p, branch))...)
	}

	st.Exec = func() error { return updateSubmodules(rctx, p, branch, targets, opts) }

	return st, len(targets), nil
}

// noSubmodulesReason explains an empty target list in the terms of what the
// repository actually has, so "nothing to do" never looks like a bug.
func noSubmodulesReason(only, untracked []string) string {
	switch {
	case len(untracked) > 0 && len(only) > 0:
		return fmt.Sprintf("submodules %v have no branch in .gitmodules, run deps freeze first", untracked)
	case len(untracked) > 0:
		return fmt.Sprintf("no submodule tracks a branch in .gitmodules (%v), run deps freeze first", untracked)
	case len(only) > 0:
		return fmt.Sprintf("none of the submodules %v are in .gitmodules and in the config", only)
	default:
		return "no submodule of this project is in the config"
	}
}

// configuredSubmodules is trackedSubmodules narrowed to what the config has
// this project follow: the submodules it lists, or — with no list — those
// another configured project provides, the same set deps freeze works on. A
// submodule outside that set is somebody else's repository: a dashboard
// bundle or a vendored service the project merely carries, which an everyday
// bump must not move. only narrows further.
func configuredSubmodules(
	rctx *run.Ctx,
	r gitx.Repo,
	p *config.Project,
	ref string,
	only []string,
) ([]submoduleTarget, []string, error) {
	scope, err := freezeList(rctx, r, p, ref)
	if err != nil {
		return nil, nil, err
	}

	paths := make([]string, 0, len(scope))
	for _, sm := range scope {
		if len(only) == 0 || slices.Contains(only, sm.Path) {
			paths = append(paths, sm.Path)
		}
	}

	if len(paths) == 0 {
		return nil, nil, nil
	}

	return trackedSubmodules(r, ref, paths)
}

// branchRef names the revision whose .gitmodules describes the run: what
// origin has for the branch, its local tip if there is no remote copy, and the
// working tree only when neither exists. Reading the checked-out .gitmodules
// instead would describe whatever branch happens to be out — on a release
// branch that means moving a frozen pin back onto the dev branch.
func branchRef(r gitx.Repo, branch string) string {
	switch {
	case r.RemoteBranchExists(branch):
		return "origin/" + branch
	case r.LocalBranchExists(branch):
		return branch
	default:
		return ""
	}
}

// refName is how a revision is named in messages; the empty ref is the
// working tree.
func refName(ref string) string {
	if ref == "" {
		return "the working tree"
	}

	return ref
}

// trackedSubmodules returns the submodules to move and, separately, the paths
// skipped because .gitmodules gives them no branch to follow. The declarations
// are read at ref, so they belong to the branch the run targets.
func trackedSubmodules(r gitx.Repo, ref string, only []string) ([]submoduleTarget, []string, error) {
	paths, err := r.SubmodulePathsAt(ref)
	if err != nil {
		return nil, nil, err
	}

	var (
		targets   []submoduleTarget
		untracked []string
	)

	for _, path := range paths {
		if len(only) > 0 && !slices.Contains(only, path) {
			continue
		}

		branch, err := r.SubmoduleBranchAt(ref, path)
		if err != nil {
			return nil, nil, err
		}

		if branch == "" {
			untracked = append(untracked, path)

			continue
		}

		targets = append(targets, submoduleTarget{Path: path, Branch: branch})
	}

	return targets, untracked, nil
}

func updateSubmodules(
	rctx *run.Ctx,
	p *config.Project,
	branch string,
	targets []submoduleTarget,
	opts submodulesOpts,
) error {
	r := repoOf(p)
	if err := startDirty(rctx, r, opts.AllowDirty); err != nil {
		return err
	}

	if err := checkoutTracking(r, branch); err != nil {
		return err
	}

	for _, target := range targets {
		// updateSubmoduleRemote already names the submodule in its errors.
		if err := updateSubmoduleRemote(rctx, p, r, target.Path, target.Branch); err != nil {
			return withLeftovers(r, err)
		}

		fmt.Printf("    %s -> head of %s\n", target.Path, planRef(target.Branch))
	}

	if !opts.NoDeps {
		if err := runDeps(rctx, r, p, branch); err != nil {
			return withLeftovers(r, err)
		}
	}

	if opts.NoCommit {
		return reportUncommitted(r)
	}

	msg := submodulesMessage(rctx, p, branch)

	return commitAndPush(rctx, r, p, cmdDepsSubmodules, branch, msg, "submodules already at the head of their branches")
}

// submodulesMessage is the commit message deps submodules would use.
func submodulesMessage(rctx *run.Ctx, p *config.Project, branch string) string {
	return expand(rctx.Cfg.SubmodulesCommitMessage, messageVars(rctx, p, branch))
}

// reportUncommitted prints what --no-commit left behind, since nothing else
// in the run will show it.
func reportUncommitted(r gitx.Repo) error {
	changed, err := r.Git("status", "--porcelain")
	if err != nil {
		return err
	}

	if changed == "" {
		fmt.Println("    submodules already at the head of their branches")

		return nil
	}

	fmt.Printf("    left uncommitted in %s:\n%s\n", r.Dir, changed)

	return nil
}
