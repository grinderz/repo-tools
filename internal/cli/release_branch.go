package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/grinderz/repo-tools/internal/config"
	"github.com/grinderz/repo-tools/internal/gitx"
	"github.com/grinderz/repo-tools/internal/run"
)

func newReleaseBranchCmd(rctx *run.Ctx, name string) *cobra.Command {
	var (
		version string
		major   bool
	)

	c := &cobra.Command{
		Use:   name + " [project...]",
		Short: "Create release branches from the dev branch and push them",
		Long: "Creates <release_branch_prefix>X.Y from the dev branch head.\n" +
			"The version defaults to a minor bump of the highest existing release branch;\n" +
			"an existing branch is reported as a warning, not an error.",
		RunE: func(cmd *cobra.Command, args []string) error {
			projects, err := rctx.Select(args)
			if err != nil {
				return err
			}

			steps := make([]run.Step, 0, len(projects))

			for _, p := range projects {
				st, err := planReleaseBranch(rctx, p, version, major)
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
	c.Flags().StringVar(
		&version,
		"version",
		"",
		"explicit release version X.Y (default: minor bump of the latest release branch)",
	)
	c.Flags().BoolVar(&major, "major", false, "bump the major version instead of the minor one")

	return c
}

func planReleaseBranch(rctx *run.Ctx, p *config.Project, version string, major bool) (run.Step, error) {
	r := repoOf(p)
	st := run.Step{Project: p}

	if reason := missingRepo(r); reason != "" {
		st.Skip, st.Warn = true, reason

		return st, nil
	}

	if err := fetch(rctx, p, r); err != nil {
		return st, fetchFailed(err)
	}

	branch, err := nextReleaseBranch(r, p, version, major)
	if err != nil {
		st.Skip, st.Warn = true, err.Error()

		return st, nil //nolint:nilerr // the error becomes the skip reason, not a failure
	}

	if r.RemoteBranchExists(branch) {
		st.Skip, st.Warn = true, fmt.Sprintf("origin/%s already exists", branch)

		return st, nil
	}

	action := "create"
	if r.LocalBranchExists(branch) {
		action = "reuse local"
	}

	st.Plan = append([]string{fmt.Sprintf("%s %s from %s and push",
		action, planRef(branch), planRef("origin/"+p.DevBranch))},
		branchSource(r, p)...)
	st.Exec = func() error { return createReleaseBranch(rctx, p, branch) }

	return st, nil
}

// branchSource describes the commit a release branch would be cut from and how
// much has landed since the previous release. Cutting a release one merge too
// late is the mistake this is here to catch.
func branchSource(r gitx.Repo, p *config.Project) []string {
	head, err := r.Git("log", "-1", "--format=%h"+fieldSep+"%s (%an, %ad)", "--date=short", "origin/"+p.DevBranch)
	if err != nil {
		return nil
	}

	short, rest, _ := strings.Cut(head, fieldSep)

	lines := make([]string, 0, 2) //nolint:mnd // a source line and a count line
	lines = append(lines, "from "+planHash(short)+" "+rest)

	branches, err := r.RemoteBranches()
	if err != nil {
		return lines
	}

	previous, _, ok := gitx.LatestReleaseBranch(branches, p.ReleaseBranchPrefix)
	if !ok {
		return lines
	}

	count, err := r.Git("rev-list", "--count", "--no-merges", "origin/"+previous+"..origin/"+p.DevBranch)
	if err != nil {
		return lines
	}

	return append(lines, fmt.Sprintf("%s commit(s) since %s", count, planRef(previous)))
}

func nextReleaseBranch(r gitx.Repo, p *config.Project, version string, major bool) (string, error) {
	if version != "" {
		name := p.ReleaseBranchPrefix + version
		if _, ok := gitx.ParseReleaseBranch(name, p.ReleaseBranchPrefix); !ok {
			return "", fmt.Errorf("version %q %w", version, errNotXY)
		}

		return name, nil
	}

	// A config that names the release branch is describing one release, so
	// that is the branch to create rather than a computed next one. --major
	// still overrides, which is how a config gets bumped past its own name.
	if p.ReleaseBranch != "" && !major {
		if _, ok := gitx.ParseReleaseBranch(p.ReleaseBranch, p.ReleaseBranchPrefix); !ok {
			return "", fmt.Errorf(
				"release_branch %q %w %sX.Y",
				p.ReleaseBranch,
				errBadReleaseName,
				p.ReleaseBranchPrefix,
			)
		}

		return p.ReleaseBranch, nil
	}

	branches, err := r.RemoteBranches()
	if err != nil {
		return "", err
	}

	_, v, ok := gitx.LatestReleaseBranch(branches, p.ReleaseBranchPrefix)
	if !ok {
		return p.ReleaseBranchPrefix + "1.0", nil
	}

	if major {
		return fmt.Sprintf("%s%d.0", p.ReleaseBranchPrefix, v.Major+1), nil
	}

	return fmt.Sprintf("%s%d.%d", p.ReleaseBranchPrefix, v.Major, v.Minor+1), nil
}

// ensureLocalBranch points branch at from. A branch that already exists is
// the leftover of a push that failed after the local step — protected
// branches, a missed key touch — and is reused as long as it carries no
// commits of its own; one that does needs a person, not a force-move.
func ensureLocalBranch(r gitx.Repo, branch, from string) error {
	if !r.LocalBranchExists(branch) {
		_, err := r.Git("branch", branch, from)

		return err
	}

	if _, err := r.Git("merge-base", "--is-ancestor", branch, from); err != nil {
		return fmt.Errorf("local %s %w %s, inspect it first",
			branch, errLocalAhead, from)
	}

	current, err := r.CurrentBranch()
	if err != nil {
		return err
	}

	// A checked-out branch cannot be force-moved; a fast-forward merge moves
	// it and keeps the working tree honest.
	if current == branch {
		_, err := r.Git("merge", "--ff-only", from)

		return err
	}

	_, err = r.Git("branch", "-f", branch, from)

	return err
}

func createReleaseBranch(rctx *run.Ctx, p *config.Project, branch string) error {
	r := repoOf(p)
	if !r.RemoteBranchExists(p.DevBranch) {
		return fmt.Errorf("origin/%s %w", p.DevBranch, errNoRemoteBranch)
	}

	ok, err := reviewBranch(rctx, r, p, branch)
	if err != nil {
		return err
	}

	if !ok {
		fmt.Printf("    %s, %s was not created in %s\n", run.Yellow("declined"), branch, r.Dir)

		return errDeclined
	}

	if err := ensureLocalBranch(r, branch, "origin/"+p.DevBranch); err != nil {
		return err
	}

	// --no-follow-tags: this push means the branch and nothing else — a
	// global push.followTags would otherwise drag every reachable tag along
	// and one stale tag would sink the branch creation.
	err = remoteGit(rctx, r, "push", "--no-follow-tags", "-u", "origin", branch)

	return err
}
