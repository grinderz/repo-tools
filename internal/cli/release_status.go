package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/grinderz/repo-tools/internal/config"
	"github.com/grinderz/repo-tools/internal/gitx"
	"github.com/grinderz/repo-tools/internal/run"
)

// The column widths of the release status table. The ci column is last and
// runs free.
const (
	statusNameWidth   = 20
	statusBranchWidth = 18
	statusFreezeWidth = 9
	statusTagWidth    = 16
	statusHeadWidth   = 14
	statusPicksWidth  = 13
	statusPinsWidth   = 10
)

// newReleaseStatusCmd prints where every project stands in the release: the
// answer to "what is left" without running four commands and keeping score by
// hand.
func newReleaseStatusCmd(rctx *run.Ctx, name string) *cobra.Command {
	var releaseBranch string

	c := &cobra.Command{
		Use:   name + " [project...]",
		Short: "Show where each project stands in the release",
		Long: "Prints one line per project: the release branch, whether the freeze\n" +
			"matches the config, the highest rc and final tag of that release line,\n" +
			"whether the branch head is tagged or has moved since, git compare's\n" +
			"counters (commits only in the dev branch / only in the release branch /\n" +
			"in both), and the state of the head's pipeline. Stale submodules and a\n" +
			"failed pipeline's URL get a detail line under the row.\n\n" +
			"Fetches first, so the picture is origin's; a project whose fetch fails\n" +
			"is skipped rather than reported from stale refs. The pipeline column\n" +
			"asks glab/gh and honours --no-ci.",
		RunE: func(_ *cobra.Command, args []string) error {
			projects, err := rctx.Select(args)
			if err != nil {
				return err
			}

			fmt.Println(run.Dim(padCell("project", statusNameWidth, nil) +
				padCell("release", statusBranchWidth, nil) +
				padCell("freeze", statusFreezeWidth, nil) +
				padCell("pins", statusPinsWidth, nil) +
				padCell("rc", statusTagWidth, nil) +
				padCell("final", statusTagWidth, nil) +
				padCell("head", statusHeadWidth, nil) +
				padCell("dev/rel/both", statusPicksWidth, nil) +
				"ci"))

			// Module-pin providers are fetched once for the whole table.
			fetched := map[string]bool{}

			for _, p := range projects {
				releaseStatusRow(rctx, p, releaseBranch, fetched)
			}

			return nil
		},
	}
	c.Flags().StringVar(
		&releaseBranch,
		"release-branch",
		"",
		"release branch to report on (default: the highest release branch on origin)",
	)

	return c
}

// padCell pads first, paints second: colour codes inside a %-9s count as
// width, so a painted cell would drag every later column out of line.
func padCell(text string, width int, paint func(string) string) string {
	padded := fmt.Sprintf("%-*s ", width, text)
	if paint == nil {
		return padded
	}

	return paint(padded)
}

func releaseStatusRow(rctx *run.Ctx, p *config.Project, override string, fetched map[string]bool) {
	r := repoOf(p)

	if reason := missingRepo(r); reason != "" {
		fmt.Println(padCell(p.Name, statusNameWidth, nil) + reason)

		return
	}

	// A dashboard built on stale refs answers yesterday's question, so a
	// failed fetch skips the project instead of shrugging.
	if reason := fetchOrWarn(rctx, p, r); reason != "" {
		fmt.Println(padCell(p.Name, statusNameWidth, nil) + run.Warn() + " " + reason + ", skipped")

		return
	}

	branch, ver, err := latestRelease(r, p, override)
	if err != nil {
		fmt.Println(padCell(p.Name, statusNameWidth, nil) + err.Error())

		return
	}

	if !r.RemoteBranchExists(branch) {
		fmt.Println(padCell(p.Name, statusNameWidth, nil) +
			padCell(branch, statusBranchWidth, run.Yellow) +
			run.Dim("pending — run release branch to create it"))

		return
	}

	freeze, freezePaint, stale := freezeStatus(rctx, r, p, branch)
	pins, pinsPaint, drifted := pinsCell(rctx, p, r, branch, fetched)
	rcTag, finalTag := releaseLineTags(r, ver)
	head, headPaint := headStatus(r, ver, branch)
	picks, picksPaint := compareCell(r, p, branch)
	ciWord, detail := ciCell(rctx, r, p, branch)

	fmt.Println(padCell(p.Name, statusNameWidth, nil) +
		padCell(branch, statusBranchWidth, nil) +
		padCell(freeze, statusFreezeWidth, freezePaint) +
		padCell(pins, statusPinsWidth, pinsPaint) +
		padCell(rcTag, statusTagWidth, nil) +
		padCell(finalTag, statusTagWidth, nil) +
		padCell(head, statusHeadWidth, headPaint) +
		padCell(picks, statusPicksWidth, picksPaint) +
		ciWord)

	for _, s := range stale {
		fmt.Println("  - " + s)
	}

	for _, s := range drifted {
		fmt.Println("  - " + s)
	}

	if detail != "" {
		fmt.Println("  " + run.Dim(detail))
	}
}

// freezeStatus says whether the release branch's .gitmodules matches what
// deps freeze would write, reusing the comparison the tag warning makes. "-"
// means there is nothing to freeze here.
func freezeStatus(
	rctx *run.Ctx,
	r gitx.Repo,
	p *config.Project,
	branch string,
) (string, func(string) string, []string) {
	if !p.DepsFreezeEnabled() {
		return "-", nil, nil
	}

	ref := branchRef(r, branch)

	submodules, err := freezeList(rctx, r, p, ref)
	if err != nil || len(submodules) == 0 {
		return "-", nil, nil
	}

	stale := unfrozenSubmodules(rctx, r, p, ref)
	if len(stale) == 0 {
		return "ok", run.Green, nil
	}

	return fmt.Sprintf("stale(%d)", len(stale)), run.Yellow, stale
}

// pinsCell folds deps status into one cell: ok while every submodule and
// module pin sits at the head of its branch, otherwise how many drifted, with
// the drifted lines repeated under the row. "-" means nothing is pinned here.
func pinsCell(
	rctx *run.Ctx,
	p *config.Project,
	r gitx.Repo,
	branch string,
	fetched map[string]bool,
) (string, func(string) string, []string) {
	reports := pinReports(rctx, p, r, branchRef(r, branch), fetched)
	if len(reports) == 0 {
		return "-", nil, nil
	}

	var drifted []string

	for _, report := range reports {
		if report.Drifted {
			drifted = append(drifted, report.Line)
		}
	}

	if len(drifted) == 0 {
		return "ok", run.Green, nil
	}

	return fmt.Sprintf("drift(%d)", len(drifted)), run.Yellow, drifted
}

// compareCell folds git compare's three groups into counters: commits only in
// the dev branch (the pending cherry-picks), only in the release branch, and
// in both. Pending picks make the cell yellow — they are the release work
// left on the table.
func compareCell(r gitx.Repo, p *config.Project, branch string) (string, func(string) string) {
	cmp, err := compareBranches(r, p, branch, pickFilter{})
	if err != nil {
		return "-", nil
	}

	text := fmt.Sprintf("%d/%d/%d", len(cmp.DevOnly), len(cmp.RelOnly), len(cmp.Both))
	if len(cmp.DevOnly) > 0 {
		return text, run.Yellow
	}

	return text, run.Green
}

// releaseLineTags is the line's highest rc and highest final tag, "-" where
// none exists yet.
func releaseLineTags(r gitx.Repo, ver gitx.ReleaseVer) (string, string) {
	rcTag, finalTag := "-", "-"

	tags, err := r.Tags()
	if err != nil {
		return rcTag, finalTag
	}

	for _, t := range gitx.TagsFor(tags, ver) {
		if t.RC >= 0 {
			rcTag = t.String()
		} else {
			finalTag = t.String()
		}
	}

	return rcTag, finalTag
}

// headStatus says whether the branch head is what the line's tags describe:
// "tagged", or how many commits arrived since the last tag — the ones the
// next rc would ship.
func headStatus(r gitx.Repo, ver gitx.ReleaseVer, branch string) (string, func(string) string) {
	tags, err := r.Tags()
	if err != nil {
		return "-", nil
	}

	line := gitx.TagsFor(tags, ver)
	if len(line) == 0 {
		return "untagged", run.Yellow
	}

	last := line[len(line)-1].String()

	count, err := r.Git("rev-list", "--count", "--no-merges", last+"..origin/"+branch)
	if err != nil {
		return "-", nil
	}

	if count == "0" {
		return "tagged", run.Green
	}

	return "+" + count + " commits", run.Yellow
}

// ciCell is the pipeline of the branch head as one table cell plus the detail
// worth its own line. "-" covers both a project without CI and --no-ci.
func ciCell(rctx *run.Ctx, r gitx.Repo, p *config.Project, branch string) (string, string) {
	if rctx.NoCI || p.CI == config.CINone {
		return "-", ""
	}

	sha, err := r.Git("rev-parse", "origin/"+branch)
	if err != nil {
		return "-", ""
	}

	pipe, err := ciQuery(r, p.CI, sha, branch)
	if err != nil {
		return run.Yellow("error"), firstLine(err.Error())
	}

	return ciSummary(pipe)
}

// ciSummary folds one pipeline into a coloured word and the detail worth a
// second line — shared by the status table and ci status.
func ciSummary(pipe ciPipeline) (string, string) {
	switch pipe.State {
	case ciMissing:
		return run.Dim("none"), ""
	case ciRunning:
		text := "running" //nolint:goconst // the state's own word, shared only with tests
		if pipe.Progress != "" {
			text += " " + pipe.Progress
		}

		return run.Yellow(text), pipe.URL
	case ciSuccess:
		return run.Green("success"), ""
	case ciFailed:
		return run.Red("failed"), pipe.URL
	case ciSkipped:
		return run.Dim("skipped"), ""
	case ciManual:
		return run.Yellow("manual"), pipe.URL
	}

	return "-", ""
}
