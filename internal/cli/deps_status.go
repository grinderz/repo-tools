package cli

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/spf13/cobra"

	"github.com/grinderz/repo-tools/internal/config"
	"github.com/grinderz/repo-tools/internal/gitx"
	"github.com/grinderz/repo-tools/internal/run"
)

// newDepsStatusCmd shows how far the internal dependencies have drifted: each
// submodule pin against the head of the branch it tracks, and each module@ref
// pin against what go.mod actually resolved — the read-only answer to "is a
// deps submodules / deps freeze run due", without planning either.
func newDepsStatusCmd(rctx *run.Ctx, name string) *cobra.Command {
	return &cobra.Command{
		Use:   name + " [project...]",
		Short: "Show how far submodule pins and module pins have drifted",
		Long: "For every submodule that tracks a branch: the recorded pin against the\n" +
			"head of that branch, with the number of commits between them. For every\n" +
			"module@ref of the deps kind's pin variable: whether the ref is the\n" +
			"branch this config would freeze it to, and how far the version in\n" +
			"go.mod is behind that branch's head in the provider's own clone.\n" +
			"Everything is read at origin's view of the target branch, so it reports\n" +
			"what is committed, not what a working tree happens to contain.\n\n" +
			"Read-only; fetches first (the submodules and providers too) unless\n" +
			"--no-fetch says otherwise.",
		RunE: func(_ *cobra.Command, args []string) error {
			projects, err := rctx.Select(args)
			if err != nil {
				return err
			}

			fetched := map[string]bool{}

			for i, p := range projects {
				if i > 0 {
					fmt.Println()
				}

				depsStatusProject(rctx, p, fetched)
			}

			return nil
		},
	}
}

func depsStatusProject(rctx *run.Ctx, p *config.Project, fetched map[string]bool) {
	header := run.Arrow() + " " + run.Bold(p.Name)
	r := repoOf(p)

	if reason := missingRepo(r); reason != "" {
		fmt.Println(header + "  " + reason)

		return
	}

	if reason := fetchOrWarn(rctx, p, r); reason != "" {
		fmt.Println(header + "  " + run.Warn() + " " + reason + ", skipped")

		return
	}

	branch := p.TargetBranch()

	ref := branchRef(r, branch)
	if ref == "" {
		fmt.Println(header + "  " + pendingReleaseWarn(branch))

		return
	}

	fmt.Println(header + "  " + run.Cyan(branch))

	reports := pinReports(rctx, p, r, ref, fetched)

	if len(reports) == 0 {
		fmt.Println("    " + run.Dim("no tracked submodules, no module pins"))

		return
	}

	for _, report := range reports {
		fmt.Println("    " + report.Line)
	}
}

// pinReport is one pin's verdict: the rendered line, and whether it needs a
// hand — anything that is not plainly in step.
type pinReport struct {
	Line    string
	Drifted bool
}

// pinReports is every pin of one project, submodules first: the shared ground
// under deps status and the release status pins column.
func pinReports(rctx *run.Ctx, p *config.Project, r gitx.Repo, ref string, fetched map[string]bool) []pinReport {
	reports := submodulePinLines(rctx, p, r, ref)

	return append(reports, modulePinLines(rctx, r, p, ref, fetched)...)
}

// submodulePinLines compares each tracked submodule's recorded pin with the
// head of the branch it tracks, inside the submodule's own clone.
func submodulePinLines(rctx *run.Ctx, p *config.Project, r gitx.Repo, ref string) []pinReport {
	targets, untracked, err := trackedSubmodules(r, ref, nil)
	if err != nil {
		return []pinReport{{Line: fmt.Sprintf("cannot read .gitmodules: %v", err), Drifted: true}}
	}

	var reports []pinReport

	for _, target := range targets {
		label := fmt.Sprintf("submodule %s @ %s: ", target.Path, planRef(target.Branch))

		pin, err := r.Git("rev-parse", ref+":"+target.Path)
		if err != nil {
			reports = append(reports, pinReport{Line: label + "no pin on " + refName(ref), Drifted: true})

			continue
		}

		drift := driftAgainst(rctx, p, gitx.Repo{Dir: filepath.Join(r.Dir, target.Path)},
			pin, target.Branch, "")
		drift.Line = label + drift.Line
		reports = append(reports, drift)
	}

	// A submodule with no branch is the freeze column's finding, not drift.
	for _, path := range untracked {
		reports = append(reports, pinReport{Line: fmt.Sprintf("submodule %s: %s", path,
			run.Dim("tracks no branch, run deps freeze first"))})
	}

	return reports
}

// driftAgainst says where sha stands relative to origin/branch in repo dir:
// in step, behind by so many commits, or off the branch entirely. detail is
// appended to the drifted states, naming where the sha came from.
func driftAgainst(rctx *run.Ctx, p *config.Project, dir gitx.Repo, sha, branch, detail string) pinReport {
	if reason := missingRepo(dir); reason != "" {
		return pinReport{Line: "not checked out, run repo sync", Drifted: true}
	}

	if rctx.WantFetchFor(p, true) {
		if _, err := dir.Git("fetch", "--prune", "origin"); err != nil {
			return pinReport{Line: run.Warn() + " fetch failed: " + gitReason(err), Drifted: true}
		}
	}

	head, err := dir.Git("rev-parse", "origin/"+branch)
	if err != nil {
		return pinReport{Line: fmt.Sprintf("origin/%s does not exist", branch), Drifted: true}
	}

	if head == sha {
		return pinReport{Line: run.Green("ok")}
	}

	behind, err := dir.Git("rev-list", "--count", sha+".."+"origin/"+branch)
	if err != nil {
		return pinReport{
			Line:    run.Yellow(shorten(sha, shortSHALen) + " is not on origin/" + branch + detail),
			Drifted: true,
		}
	}

	if off, err := dir.Git("rev-list", "--count", "origin/"+branch+".."+sha); err == nil && off != "0" {
		return pinReport{
			Line:    run.Yellow(fmt.Sprintf("diverged: %s commit(s) off origin/%s%s", off, branch, detail)),
			Drifted: true,
		}
	}

	return pinReport{
		Line:    run.Yellow(fmt.Sprintf("behind %s commit(s)%s", behind, detail)),
		Drifted: true,
	}
}

// modulePin versions in go.mod: a pseudo-version carries the commit it
// resolved, a plain version names a tag.
var pseudoVersionRe = regexp.MustCompile(`-\d{14}-([0-9a-f]{12})$`)

// modulePinLines walks the deps kind's module@ref list the way the freeze
// does, and reports each module against the provider project's own clone:
// whether the ref is the branch this config would pin, and how far go.mod's
// resolved version is behind that branch's head.
func modulePinLines(
	rctx *run.Ctx,
	r gitx.Repo,
	p *config.Project,
	ref string,
	fetched map[string]bool,
) []pinReport {
	pin, ok := rctx.Cfg.DepsPinsFor(p)
	if !ok {
		return nil
	}

	text, found, err := r.FileAt(ref, pin.File)
	if err != nil || !found {
		return []pinReport{{
			Line:    fmt.Sprintf("%s: not on %s, module pins unknown", pin.File, refName(ref)),
			Drifted: true,
		}}
	}

	fileLines := strings.Split(text, "\n")

	first, last, ok := variableLines(fileLines, pin.Var)
	if !ok {
		return []pinReport{{
			Line:    fmt.Sprintf("%s: no %s, module pins unknown", pin.File, pin.Var),
			Drifted: true,
		}}
	}

	gomod, _, _ := r.FileAt(ref, "go.mod")

	var reports []pinReport

	for i := first; i <= last; i++ {
		for _, match := range modulePin.FindAllString(fileLines[i], -1) {
			module, pinBranch, _ := strings.Cut(match, "@")
			reports = append(reports, moduleLine(rctx, module, pinBranch, gomod, fetched))
		}
	}

	return reports
}

func moduleLine(
	rctx *run.Ctx,
	module, pinBranch, gomod string,
	fetched map[string]bool,
) pinReport {
	label := fmt.Sprintf("module %s @ %s: ", module, planRef(pinBranch))

	provider := rctx.Cfg.ProjectByGit(module)
	if provider == nil {
		return pinReport{Line: label + run.Yellow("not a project in this config"), Drifted: true}
	}

	if want := provider.TargetBranch(); want != pinBranch {
		return pinReport{Line: label + run.Yellow("config would pin "+want+", run deps freeze"), Drifted: true}
	}

	version := goModVersion(gomod, module)
	if version == "" {
		// A module the project does not consume through go.mod says nothing
		// about drift.
		return pinReport{Line: label + run.Dim("not in go.mod")}
	}

	providerRepo := repoOf(provider)

	if reason := missingRepo(providerRepo); reason != "" {
		return pinReport{Line: label + version + ", provider " + reason, Drifted: true}
	}

	// The provider is fetched once for the whole run, not once per consumer.
	if !fetched[provider.Name] {
		fetched[provider.Name] = true

		if reason := fetchOrWarn(rctx, provider, providerRepo); reason != "" {
			return pinReport{Line: label + run.Warn() + " provider " + reason, Drifted: true}
		}
	}

	sha, err := versionCommit(providerRepo, version)
	if err != nil {
		return pinReport{
			Line: label + run.Yellow(
				fmt.Sprintf("go.mod has %s, which %s's clone does not know", version, provider.Name),
			),
			Drifted: true,
		}
	}

	rctxNoFetch := *rctx
	rctxNoFetch.FetchFlag = new(bool) // the provider was fetched above

	drift := driftAgainst(&rctxNoFetch, provider, providerRepo, sha, pinBranch, " (go.mod "+version+")")
	drift.Line = label + drift.Line

	return drift
}

// goModVersion is the version go.mod requires for one module, "" when the
// module is not there. The module sits either on its own line of a require
// block or right after the require keyword.
func goModVersion(gomod, module string) string {
	re := regexp.MustCompile(`(?m)(?:^|\s)` + regexp.QuoteMeta(module) + `\s+(v\S+)`)

	m := re.FindStringSubmatch(gomod)
	if m == nil {
		return ""
	}

	return m[1]
}

// versionCommit resolves a go.mod version to the commit it pinned: the commit
// baked into a pseudo-version, or the tag's target for a released version.
func versionCommit(r gitx.Repo, version string) (string, error) {
	if m := pseudoVersionRe.FindStringSubmatch(version); m != nil {
		return r.Git("rev-parse", m[1]+"^{commit}")
	}

	// The repositories tag without the leading v; try both spellings.
	if sha, err := r.Git("rev-parse", strings.TrimPrefix(version, "v")+"^{commit}"); err == nil {
		return sha, nil
	}

	return r.Git("rev-parse", version+"^{commit}")
}
