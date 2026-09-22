package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/grinderz/repo-tools/internal/config"
	"github.com/grinderz/repo-tools/internal/gitx"
	"github.com/grinderz/repo-tools/internal/run"
)

// newCompareCmd shows, per project, how the dev branch and the release branch
// have diverged commit by commit — the work overview to read before deciding
// what to cherry-pick.
func newCompareCmd(rctx *run.Ctx, name string) *cobra.Command {
	var (
		releaseBranch string
		tasks, greps  []string
		since, until  string
	)

	c := &cobra.Command{
		Use:   name + " [project...]",
		Short: "Show how the dev and release branches differ, commit by commit",
		Long: "Compares each project's dev branch with its release branch and sorts\n" +
			"the commits since they diverged into three groups: only in the dev\n" +
			"branch (the cherry-pick candidates), only in the release branch (freeze\n" +
			"commits, version bumps, direct hotfixes), and in both — either as an\n" +
			"equivalent patch or through the -x trailer of an earlier cherry-pick,\n" +
			"where the two hashes are shown as a pair. Fetches first, so the picture\n" +
			"is origin's, not the clone's; a project whose fetch fails is skipped\n" +
			"rather than compared against stale refs.\n\n" +
			"--task, --grep, --since and --until narrow every group, so the table\n" +
			"can answer for one ticket or one time window. Read-only: nothing is\n" +
			"picked, nothing is written.",
		RunE: func(_ *cobra.Command, args []string) error {
			filter, err := newPickFilter(tasks, greps, since, until)
			if err != nil {
				return err
			}

			projects, err := rctx.Select(args)
			if err != nil {
				return err
			}

			for i, p := range projects {
				if i > 0 {
					fmt.Println()
				}

				compareProject(rctx, p, releaseBranch, filter)
			}

			return nil
		},
	}
	c.Flags().StringVar(
		&releaseBranch,
		"release-branch",
		"",
		"release branch to compare against (default: the highest release branch on origin)",
	)
	c.Flags().StringSliceVar(
		&tasks,
		"task",
		nil,
		"keep only commits whose subject mentions these ticket ids (comma separated or repeated)",
	)
	c.Flags().StringArrayVar(
		&greps,
		"grep",
		nil,
		"keep only commits whose subject matches this regexp (repeatable, case insensitive)",
	)
	c.Flags().StringVar(&since, "since", "", "keep only commits from this date onwards (any date git understands)")
	c.Flags().StringVar(&until, "until", "", "keep only commits up to this date")

	return c
}

func compareProject(rctx *run.Ctx, p *config.Project, releaseBranch string, filter pickFilter) {
	header := run.Arrow() + " " + run.Bold(p.Name)
	r := repoOf(p)

	if reason := missingRepo(r); reason != "" {
		fmt.Println(header + "  " + reason)

		return
	}

	// Comparing stale refs would present yesterday's plan as today's, so a
	// failed fetch skips the project instead of shrugging.
	if reason := fetchOrWarn(rctx, p, r); reason != "" {
		fmt.Println(header + "  " + run.Warn() + " " + reason + ", skipped")

		return
	}

	branch, _, err := latestRelease(r, p, releaseBranch)
	if err != nil {
		fmt.Printf("%s  %v\n", header, err)

		return
	}

	cmp, err := compareBranches(r, p, branch, filter)
	if err != nil {
		fmt.Printf("%s  %v\n", header, err)

		return
	}

	fmt.Println(header + "  " + run.Cyan(p.DevBranch) + " <-> " + run.Cyan(branch))

	if !filter.empty() {
		fmt.Println(" " + run.Dim("selected by ["+filter.describe()+"], oldest first"))
	}

	printCompareGroup("only in "+p.DevBranch, plainLines(cmp.DevOnly))
	printCompareGroup("only in "+branch, plainLines(cmp.RelOnly))

	both := make([]string, 0, len(cmp.Both))
	for _, c := range cmp.Both {
		both = append(both, run.Dim(fmt.Sprintf("%s\t%s\t%s  (%s)", c.Short, c.Author, c.Subject, c.How)))
	}

	printCompareGroup("in both", both)
}

// plainLines renders one group's commits the way the cherry-pick list does.
func plainLines(cands []candidate) []string {
	lines := make([]string, 0, len(cands))
	for _, c := range cands {
		lines = append(lines, c.String())
	}

	return lines
}

func printCompareGroup(title string, lines []string) {
	fmt.Printf(" %s (%d):\n", run.Bold(title), len(lines))

	if len(lines) == 0 {
		fmt.Println("   " + run.Dim("none"))

		return
	}

	for _, line := range lines {
		fmt.Println("   " + line)
	}
}

// sharedCandidate is a commit both branches carry, with how the release side
// got it.
type sharedCandidate struct {
	candidate

	How string
}

// comparison sorts the commits made since the branches diverged. Commits from
// before the fork point are common history and stay out of the picture.
type comparison struct {
	DevOnly []candidate       // still to cherry-pick
	RelOnly []candidate       // the release's own: freezes, bumps, hotfixes
	Both    []sharedCandidate // already carried over, shown from the dev side
}

// compareBranches classifies both sides of dev...release. A dev commit counts
// as shared when a release commit records it in a -x trailer or carries an
// equivalent patch (git cherry); the release copy is then hidden so the pair
// appears once. Everything else is genuinely one-sided.
func compareBranches(r gitx.Repo, p *config.Project, branch string, filter pickFilter) (comparison, error) {
	if !r.RemoteBranchExists(branch) {
		return comparison{}, pendingRelease(branch)
	}

	dev, rel := "origin/"+p.DevBranch, "origin/"+branch

	pairs, err := pickedPairs(r, p, branch)
	if err != nil {
		return comparison{}, err
	}

	devEquiv, err := cherryEquivalents(r, rel, dev)
	if err != nil {
		return comparison{}, err
	}

	relEquiv, err := cherryEquivalents(r, dev, rel)
	if err != nil {
		return comparison{}, err
	}

	devCands, err := rangeLog(r, rel+".."+dev, filter)
	if err != nil {
		return comparison{}, err
	}

	relCands, err := rangeLog(r, dev+".."+rel, filter)
	if err != nil {
		return comparison{}, err
	}

	var cmp comparison

	// copies are release commits shown from the dev side instead: the ones
	// whose -x trailer names a commit that really is in the dev range. A
	// trailer pointing elsewhere hides nothing.
	copies := map[string]bool{}

	for _, c := range devCands {
		if copySHA, ok := pairs[c.SHA]; ok {
			copies[copySHA] = true
		} else if copySHA, ok := pairs[c.Short]; ok {
			copies[copySHA] = true
		}
	}

	for _, c := range devCands {
		if !filter.matches(c.Subject) {
			continue
		}

		switch {
		case pairs[c.SHA] != "":
			cmp.Both = append(cmp.Both, sharedCandidate{c, "picked as " + shorten(pairs[c.SHA], shortSHALen)})
		case pairs[c.Short] != "":
			cmp.Both = append(cmp.Both, sharedCandidate{c, "picked as " + shorten(pairs[c.Short], shortSHALen)})
		case devEquiv[c.SHA]:
			cmp.Both = append(cmp.Both, sharedCandidate{c, "equivalent patch in " + branch})
		default:
			cmp.DevOnly = append(cmp.DevOnly, c)
		}
	}

	for _, c := range relCands {
		if !filter.matches(c.Subject) || copies[c.SHA] || relEquiv[c.SHA] {
			continue
		}

		cmp.RelOnly = append(cmp.RelOnly, c)
	}

	return cmp, nil
}

// cherryEquivalents is the set of commits in upstream..head whose patch
// upstream already carries: the "-" lines of git cherry.
func cherryEquivalents(r gitx.Repo, upstream, head string) (map[string]bool, error) {
	lines, err := r.Lines("cherry", upstream, head)
	if err != nil {
		return nil, err
	}

	equiv := map[string]bool{}

	for _, l := range lines {
		fields := strings.Fields(l)
		if len(fields) == cherryFields && fields[0] == "-" {
			equiv[fields[1]] = true
		}
	}

	return equiv, nil
}
