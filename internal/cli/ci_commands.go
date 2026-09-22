package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/grinderz/repo-tools/internal/config"
	"github.com/grinderz/repo-tools/internal/gitx"
	"github.com/grinderz/repo-tools/internal/run"
)

// The push commands watch their own pipelines; these two look at a pipeline
// without pushing anything — after an aborted run, a manual push, or just to
// ask "is everything green?" before the final tag.

func newCIStatusCmd(rctx *run.Ctx, name string) *cobra.Command {
	var ref string

	c := &cobra.Command{
		Use:   name + " [project...]",
		Short: "Show the pipeline state of each project's head",
		Long: "Asks glab/gh about the pipeline of each project's target branch head\n" +
			"— the release branch when the config names one, the dev branch\n" +
			"otherwise — and prints one line per project. --ref points it at another\n" +
			"branch or at a tag instead. Projects with ci: none are listed as not\n" +
			"configured. Read-only: nothing is retried, nothing is pushed.",
		RunE: func(_ *cobra.Command, args []string) error {
			projects, err := rctx.Select(args)
			if err != nil {
				return err
			}

			for _, p := range projects {
				ciStatusLine(rctx, p, ref)
			}

			return nil
		},
	}
	c.Flags().StringVar(&ref, "ref", "", "branch or tag to look at (default: the project's target branch)")

	return c
}

func ciStatusLine(rctx *run.Ctx, p *config.Project, override string) {
	r := repoOf(p)

	if reason := missingRepo(r); reason != "" {
		fmt.Println(padCell(p.Name, statusNameWidth, nil) + reason)

		return
	}

	if rctx.NoCI || p.CI == config.CINone {
		fmt.Println(padCell(p.Name, statusNameWidth, nil) + run.Dim("ci is not configured"))

		return
	}

	if reason := fetchOrWarn(rctx, p, r); reason != "" {
		fmt.Println(padCell(p.Name, statusNameWidth, nil) + run.Warn() + " " + reason + ", skipped")

		return
	}

	sha, ref, err := ciTarget(r, p, override)
	if err != nil {
		fmt.Println(padCell(p.Name, statusNameWidth, nil) + err.Error())

		return
	}

	pipe, err := ciQuery(r, p.CI, sha, ref)
	if err != nil {
		fmt.Println(padCell(p.Name, statusNameWidth, nil) +
			padCell(ref, statusBranchWidth, nil) + firstLine(err.Error()))

		return
	}

	word, detail := ciSummary(pipe)
	line := padCell(p.Name, statusNameWidth, nil) + padCell(ref, statusBranchWidth, nil) + word

	if pipe.Label != "" {
		line += "  " + pipe.Label
	}

	if detail != "" {
		line += "  " + run.Dim(detail)
	}

	fmt.Println(line)
}

func newCIWatchCmd(rctx *run.Ctx, name string) *cobra.Command {
	var ref string

	c := &cobra.Command{
		Use:   name + " [project...]",
		Short: "Watch the pipeline of each project's head until it settles",
		Long: "The same watch that follows every push — poll interval, job progress\n" +
			"and ci_retries included — attached to a revision that is already on\n" +
			"origin: the target branch head by default, any branch or tag via --ref.\n" +
			"For picking a watch back up after an aborted run or a manual push. A\n" +
			"failed pipeline (after the retries) fails the command.",
		RunE: func(_ *cobra.Command, args []string) error {
			projects, err := rctx.Select(args)
			if err != nil {
				return err
			}

			failed := 0

			for _, p := range projects {
				if err := ciWatchProject(rctx, p, ref); err != nil {
					if !rctx.KeepGoing {
						return err
					}

					fmt.Printf("    %s %v\n", run.Fail(), err)

					failed++
				}
			}

			if failed > 0 {
				return fmt.Errorf("%d %w", failed, errProjectsFailed)
			}

			return nil
		},
	}
	c.Flags().StringVar(&ref, "ref", "", "branch or tag to watch (default: the project's target branch)")

	return c
}

func ciWatchProject(rctx *run.Ctx, p *config.Project, override string) error {
	r := repoOf(p)

	if reason := missingRepo(r); reason != "" {
		return fmt.Errorf("%s: %w", p.Name, reasonError(reason))
	}

	if p.CI == config.CINone {
		fmt.Printf("%s %s  %s\n", run.Arrow(), run.Bold(p.Name), run.Dim("ci is not configured, skipped"))

		return nil
	}

	// Watching the wrong commit quietly reports the wrong pipeline, so a
	// failed fetch is an error here, not a warning.
	if err := fetch(rctx, p, r); err != nil {
		return fmt.Errorf("%s: %w", p.Name, fetchFailed(err))
	}

	sha, ref, err := ciTarget(r, p, override)
	if err != nil {
		return fmt.Errorf("%s: %w", p.Name, err)
	}

	fmt.Printf(
		"%s %s  %s @ %s\n",
		run.Arrow(),
		run.Bold(p.Name),
		run.Cyan(ref),
		planHash(shorten(sha, shortSHALen)),
	)

	if err := watchCI(rctx, r, p, sha, ref, ""); err != nil {
		return fmt.Errorf("%s: %w", p.Name, err)
	}

	return nil
}

// ciTarget resolves what to ask CI about: an explicit --ref — a branch on
// origin or a tag — or the project's target branch.
func ciTarget(r gitx.Repo, p *config.Project, override string) (string, string, error) {
	ref := override
	if ref == "" {
		ref = p.TargetBranch()
	}

	if r.RemoteBranchExists(ref) {
		sha, err := r.Git("rev-parse", "origin/"+ref)

		return sha, ref, err
	}

	if tagExists(r, ref) {
		sha, err := r.Git("rev-parse", ref+"^{commit}")

		return sha, ref, err
	}

	return "", "", fmt.Errorf("%q %w", ref, errUnknownRef)
}
