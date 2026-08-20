package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/grinderz/repo-tools/internal/config"
	"github.com/grinderz/repo-tools/internal/gitx"
	"github.com/grinderz/repo-tools/internal/run"
)

// newPruneCmd deletes the local branches that only get in the way: leftovers
// of failed pushes and branches whose upstream is gone. A stale local
// release-X.Y is exactly what once greeted a rerun with "a branch named
// 'release-0.2' already exists".
func newPruneCmd(rctx *run.Ctx, name string) *cobra.Command {
	return &cobra.Command{
		Use:   name + " [project...]",
		Short: "Delete local branches that are safe to lose",
		Long: "Deletes, per project, the local branches nothing would miss: branches\n" +
			"whose upstream is gone from origin but whose commits some remote branch\n" +
			"still carries, and local release branches that sit at or behind their\n" +
			"origin counterpart with no commits of their own. The current branch and\n" +
			"the dev branch are never touched; a branch with commits nobody else\n" +
			"has is kept and named, not deleted. Fetches (with --prune) first, so\n" +
			"\"gone\" is judged against origin as it is now.",
		RunE: func(cmd *cobra.Command, args []string) error {
			projects, err := rctx.Select(args)
			if err != nil {
				return err
			}

			steps := make([]run.Step, 0, len(projects))

			for _, p := range projects {
				st, err := planPrune(rctx, p)
				if err != nil {
					return fmt.Errorf("%s: %w", p.Name, err)
				}

				steps = append(steps, st)
			}

			ok, err := rctx.Gate(cmdLabel(cmd), run.Destructive, steps)
			if err != nil || !ok {
				return err
			}

			return run.Execute(steps, !rctx.KeepGoing)
		},
	}
}

// pruneTarget is one branch the prune would delete, with the reason it is
// safe.
type pruneTarget struct {
	Name   string
	Reason string
}

func planPrune(rctx *run.Ctx, p *config.Project) (run.Step, error) {
	r := repoOf(p)
	st := run.Step{Project: p}

	if reason := missingRepo(r); reason != "" {
		st.Skip, st.Warn = true, reason

		return st, nil
	}

	// "gone" is a statement about origin, so origin must be current; the
	// usual --prune fetch also drops the remote-tracking refs of deleted
	// branches, which is the very signal this command reads.
	if err := fetch(rctx, p, r); err != nil {
		return st, fetchFailed(err)
	}

	targets, kept, err := pruneTargets(r, p)
	if err != nil {
		st.Skip, st.Warn = true, err.Error()

		return st, nil //nolint:nilerr // the error becomes the skip reason, not a failure
	}

	if len(targets) == 0 {
		st.Skip, st.Warn = true, "nothing to prune"

		if len(kept) > 0 {
			st.Warn = "nothing safe to prune; kept: " + strings.Join(kept, "; ")
		}

		return st, nil
	}

	for _, target := range targets {
		st.Plan = append(st.Plan, fmt.Sprintf("delete local branch %s (%s)", planRef(target.Name), target.Reason))
	}

	st.Warn = strings.Join(kept, "; ")
	st.Exec = func() error { return pruneBranches(r, targets) }

	return st, nil
}

// pruneTargets sorts the local branches into deletable and kept-with-reason.
// The current branch and the dev branch are not even candidates.
func pruneTargets(r gitx.Repo, p *config.Project) ([]pruneTarget, []string, error) {
	current, err := r.CurrentBranch()
	if err != nil {
		current = ""
	}

	lines, err := r.Lines("for-each-ref",
		"--format=%(refname:short)"+fieldSep+"%(upstream:short)"+fieldSep+"%(upstream:track)",
		"refs/heads")
	if err != nil {
		return nil, nil, err
	}

	var (
		targets []pruneTarget
		kept    []string
	)

	for _, line := range lines {
		parts := strings.SplitN(line, fieldSep, 3) //nolint:mnd // the three fields of the format above
		name := parts[0]

		if name == current || name == p.DevBranch {
			continue
		}

		upstream, track := "", ""
		if len(parts) > 1 {
			upstream = parts[1]
		}

		if len(parts) > 2 { //nolint:mnd // see above
			track = parts[2]
		}

		target, keep := judgeBranch(r, p, name, upstream, track)
		if target != nil {
			targets = append(targets, *target)
		}

		if keep != "" {
			kept = append(kept, keep)
		}
	}

	return targets, kept, nil
}

// judgeBranch decides one branch's fate: delete with a reason, keep with a
// warning, or none of anyone's business.
func judgeBranch(r gitx.Repo, p *config.Project, name, upstream, track string) (*pruneTarget, string) {
	// Upstream deleted on origin: the branch's work is either preserved on
	// some remote branch, or it is the only copy and stays.
	if upstream != "" && strings.Contains(track, "[gone]") {
		if remoteContains(r, name) {
			return &pruneTarget{Name: name, Reason: "upstream is gone, the commits are on origin"}, ""
		}

		return nil, name + " has commits no remote branch carries, inspect it first"
	}

	// A local release branch: a leftover of release branch/cherry-pick runs.
	// In step with origin means it holds nothing of its own and any command
	// recreates it on demand.
	if _, ok := gitx.ParseReleaseBranch(name, p.ReleaseBranchPrefix); !ok {
		return nil, ""
	}

	if !r.RemoteBranchExists(name) {
		if remoteContains(r, name) {
			return &pruneTarget{Name: name, Reason: "never made it to origin, the commits are there anyway"}, ""
		}

		return nil, name + " is not on origin and its commits are nowhere else, inspect it first"
	}

	if _, err := r.Git("merge-base", "--is-ancestor", name, "origin/"+name); err == nil {
		return &pruneTarget{Name: name, Reason: "at or behind origin/" + name + ", no commits of its own"}, ""
	}

	return nil, name + " has commits that are not on origin/" + name + ", inspect it first"
}

// remoteContains says whether any remote branch already carries the branch's
// head — the test that makes deleting a local ref lose nothing.
func remoteContains(r gitx.Repo, name string) bool {
	out, err := r.Git("branch", "-r", "--contains", name)

	return err == nil && strings.TrimSpace(out) != ""
}

func pruneBranches(r gitx.Repo, targets []pruneTarget) error {
	for _, target := range targets {
		if _, err := r.Git("branch", "-D", target.Name); err != nil {
			return err
		}

		fmt.Printf("    deleted %s\n", planRef(target.Name))
	}

	return nil
}
