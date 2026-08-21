package cli

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/spf13/cobra"

	"github.com/grinderz/repo-tools/internal/config"
	"github.com/grinderz/repo-tools/internal/gitx"
	"github.com/grinderz/repo-tools/internal/run"
)

// cherryFields is the field count of a "git cherry" line: mark and hash.
const cherryFields = 2

// noEditorEnv keeps git from opening an editor on --continue.
func noEditorEnv() []string { return []string{"GIT_EDITOR=true"} }

func newCherryPickCmd(rctx *run.Ctx, name string) *cobra.Command {
	var (
		list          bool
		releaseBranch string
		tasks, greps  []string
		since, until  string
	)

	c := &cobra.Command{
		Use:   name + " [project] [sha...]",
		Short: "Cherry-pick commits from the dev branch into the release branch",
		Long: "Cherry-picks the given commits of one project into its release branch\n" +
			"with -x and pushes. Given a project but no commits, it lists the ones\n" +
			"missing from the release branch and asks which to take. A conflicting\n" +
			"submodule pin is resolved in favour of the release freeze (git submodule\n" +
			"update --remote), any other conflict stops the run with the cherry-pick\n" +
			"left in progress.\n\n" +
			"--task and --grep narrow the candidates to whole tickets or a pattern;\n" +
			"with either of them the matching commits are taken without asking.\n" +
			"--since and --until scope the list by commit date and still leave the\n" +
			"choice to you.\n\n" +
			"With --list: reports, per project, the dev-branch commits that are not in\n" +
			"the release branch yet, without picking anything.",
		RunE: func(cmd *cobra.Command, args []string) error {
			filter, err := newPickFilter(tasks, greps, since, until)
			if err != nil {
				return err
			}

			if list {
				return listCandidates(rctx, args, releaseBranch, filter)
			}

			return runCherryPick(rctx, cmdLabel(cmd), args, releaseBranch, filter)
		},
	}
	c.Flags().BoolVar(&list, "list", false, "list dev-branch commits missing from the release branch")
	c.Flags().StringVar(
		&releaseBranch,
		"release-branch",
		"",
		"target release branch (default: the highest release branch on origin)",
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

func runCherryPick(rctx *run.Ctx, name string, args []string, releaseBranch string, filter pickFilter) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: %s <project> [sha...] (or --list)", name)
	}

	projects, err := rctx.Select(args[:1])
	if err != nil {
		return err
	}

	p := projects[0]
	r := repoOf(p)

	if reason := missingRepo(r); reason != "" {
		return fmt.Errorf("%s: %s", p.Name, reason) //nolint:err113 // human-facing, not matched by callers
	}

	if err := fetch(rctx, p, r); err != nil {
		return err
	}

	branch, _, err := latestRelease(r, p, releaseBranch)
	if err != nil {
		return err
	}

	// Commits come from the command line, from the filter, or from the picker.
	var resolved []string
	if len(args) == 1 {
		resolved, err = selectCandidates(rctx, r, p, branch, filter)
	} else {
		resolved, err = resolveShas(r, p, branch, args[1:])
	}

	if err != nil {
		return err
	}

	steps := []run.Step{{
		Project: p,
		Plan: []string{
			"checkout " + planRef(branch),
			"cherry-pick -x " + strings.Join(resolved, " "),
			pushPlanLine(rctx, p, branch),
		},
		Exec: func() error { return cherryPick(rctx, p, branch, resolved) },
	}}

	ok, err := rctx.Gate(name, run.Destructive, steps)
	if err != nil || !ok {
		return err
	}

	return run.Execute(steps, !rctx.KeepGoing)
}

// selectCandidates resolves which commits to pick when none were named on the
// command line: everything a --task/--grep filter matches, or whatever the
// operator chooses from the printed list. Either way they keep their
// chronological order, which is the order they have to be picked in.
func selectCandidates(
	rctx *run.Ctx,
	r gitx.Repo,
	p *config.Project,
	branch string,
	filter pickFilter,
) ([]string, error) {
	set, err := candidatesFor(r, p, branch, filter)
	if err != nil {
		return nil, err
	}

	cands := set.Picking
	if len(cands) == 0 {
		return nil, emptySelection(p, branch, filter, set.AlreadyInSync)
	}

	printCandidates(p, branch, set, filter, rctx.Interactive() && !filter.selects())

	// Subject terms are themselves the selection; the plan and its confirmation
	// still show every commit before anything happens.
	if filter.selects() {
		return shasOf(cands), nil
	}

	if !rctx.Interactive() {
		return nil, fmt.Errorf(
			"%s: no commits given, and choosing them needs a question this run cannot ask "+
				"(--yes or confirm: never): pass the hashes, use --task/--grep, or use --confirm",
			p.Name,
		)
	}

	answer, err := run.Prompt(run.Bold("pick") +
		run.Dim(" (e.g. 1 3, 2-4, "+selectAll+"; empty to abort)") + ": ")
	if err != nil {
		return nil, err
	}

	indexes, err := parseSelection(answer, len(cands))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", p.Name, err)
	}

	picked := make([]string, 0, len(indexes))
	for _, i := range indexes {
		picked = append(picked, cands[i].SHA)
	}

	return picked, nil
}

// emptySelection explains why nothing is left to pick: the release branch has
// it all already, or nothing matched in the first place.
func emptySelection(p *config.Project, branch string, filter pickFilter, alreadyInSync int) error {
	switch {
	case filter.empty():
		return fmt.Errorf("%s: %s already has everything from %s", p.Name, branch, p.DevBranch)
	case alreadyInSync > 0:
		return fmt.Errorf("%s: all %d commit(s) selected by [%s] are already in %s",
			p.Name, alreadyInSync, filter.describe(), branch)
	default:
		return fmt.Errorf("%s: nothing in %s is selected by [%s]", p.Name, p.DevBranch, filter.describe())
	}
}

// printCandidates lists the commits, numbered when they are about to be chosen
// by hand.
func printCandidates(p *config.Project, branch string, set candidateSet, filter pickFilter, numbered bool) {
	cands := set.Picking

	header := fmt.Sprintf("%s %s  %s <- %s: %d candidate(s)",
		run.Cyan("==>"), run.Bold(p.Name), run.Cyan(branch), run.Cyan(p.DevBranch), len(cands))
	if !filter.empty() {
		header += " selected by [" + filter.describe() + "]"
	}

	if set.AlreadyInSync > 0 {
		header += fmt.Sprintf(" (%d already in %s)", set.AlreadyInSync, branch)
	}

	fmt.Println(header + ", oldest first")

	for num, cand := range cands {
		if numbered {
			fmt.Printf("  %3d  %s\n", num+1, cand)

			continue
		}

		fmt.Printf("  %s\n", cand)
	}
}

// resolveShas verifies each sha exists and expands it to a full hash, keeping
// the order given on the command line. Commits the release branch already
// carries are warned about and dropped here, before anything is touched: git
// would otherwise report them as an empty pick, or as a conflict if the release
// copy was edited afterwards.
func resolveShas(r gitx.Repo, p *config.Project, branch string, shas []string) ([]string, error) {
	picked, err := alreadyPicked(r, p, branch)
	if err != nil {
		return nil, err
	}

	out := make([]string, 0, len(shas))

	for _, s := range shas {
		full, err := r.Git("rev-parse", "--verify", s+"^{commit}")
		if err != nil {
			return nil, fmt.Errorf("%s: unknown commit %q", p.Name, s)
		}

		if reason := alreadyInBranch(r, branch, full, picked); reason != "" {
			fmt.Printf("%s %s: %s is already in %s (%s), skipping\n",
				run.Warn(), p.Name, shorten(full, shortSHALen), branch, reason)

			continue
		}

		out = append(out, full)
	}

	if len(out) == 0 {
		return nil, fmt.Errorf("%s: every commit given is already in %s", p.Name, branch)
	}

	return out, nil
}

// alreadyInBranch reports how branch already carries sha, if it does: as the
// very commit, as a recorded cherry-pick source, or as an equivalent patch.
func alreadyInBranch(r gitx.Repo, branch, sha string, picked map[string]bool) string {
	if picked[sha] || picked[shorten(sha, shortSHALen)] {
		return "cherry picked from it before"
	}

	if _, err := r.Git("merge-base", "--is-ancestor", sha, "origin/"+branch); err == nil {
		return "the commit itself is an ancestor"
	}

	// git cherry marks a patch-equivalent commit with "-". The limit argument
	// is what keeps it to this one commit instead of its whole history.
	out, err := r.Git("cherry", "origin/"+branch, sha, sha+"^")
	if err == nil && strings.HasPrefix(out, "-") {
		return "an equivalent patch is there"
	}

	return ""
}

var pickedFromRe = regexp.MustCompile(`cherry picked from commit ([0-9a-f]{7,40})`)

// alreadyPicked collects source hashes recorded by cherry-pick -x in branch.
func alreadyPicked(r gitx.Repo, p *config.Project, branch string) (map[string]bool, error) {
	pairs, err := pickedPairs(r, p, branch)
	if err != nil {
		return nil, err
	}

	picked := make(map[string]bool, len(pairs))
	for sha := range pairs {
		picked[sha] = true
	}

	return picked, nil
}

// pickedPairs maps each source hash recorded by a cherry-pick -x trailer in
// branch to the commit carrying the trailer. Only the commits the branch does
// not share with the dev branch can carry one, and reading just those keeps
// this off the whole history. NUL-separated records survive multi-line bodies.
func pickedPairs(r gitx.Repo, p *config.Project, branch string) (map[string]string, error) {
	out, err := r.Git("log", "--no-merges", "-z", "--format=%H"+fieldSep+"%B",
		"origin/"+p.DevBranch+"..origin/"+branch)
	if err != nil {
		return nil, err
	}

	pairs := map[string]string{}

	for record := range strings.SplitSeq(out, "\x00") {
		sha, body, ok := strings.Cut(record, fieldSep)
		if !ok {
			continue
		}

		for _, m := range pickedFromRe.FindAllStringSubmatch(body, -1) {
			pairs[m[1]] = sha
		}
	}

	return pairs, nil
}

// candidates returns dev-branch commits missing from the release branch:
// git cherry drops patch-equivalent ones, the -x trailers drop the rest.
func candidates(r gitx.Repo, p *config.Project, branch string) ([]string, error) {
	lines, err := r.Lines("cherry", "origin/"+branch, "origin/"+p.DevBranch)
	if err != nil {
		return nil, err
	}

	picked, err := alreadyPicked(r, p, branch)
	if err != nil {
		return nil, err
	}

	out := make([]string, 0, len(lines))

	for _, l := range lines {
		fields := strings.Fields(l)
		if len(fields) != cherryFields || fields[0] != "+" {
			continue
		}

		sha := fields[1]
		if picked[sha] || picked[shorten(sha, shortSHALen)] {
			continue
		}

		out = append(out, sha)
	}

	return out, nil
}

func listCandidates(rctx *run.Ctx, args []string, releaseBranch string, filter pickFilter) error {
	projects, err := rctx.Select(args)
	if err != nil {
		return err
	}

	for i, p := range projects {
		if i > 0 {
			fmt.Println()
		}

		listProjectCandidates(rctx, p, releaseBranch, filter)
	}

	return nil
}

func listProjectCandidates(rctx *run.Ctx, p *config.Project, releaseBranch string, filter pickFilter) {
	header := run.Cyan("==>") + " " + run.Bold(p.Name)
	r := repoOf(p)

	if reason := missingRepo(r); reason != "" {
		fmt.Println(header + "  " + reason)

		return
	}

	if reason := fetchOrWarn(rctx, p, r); reason != "" {
		fmt.Println(header + "  " + run.Warn() + " " + reason + ", skipped")

		return
	}

	branch, _, err := latestRelease(r, p, releaseBranch)
	if err != nil {
		fmt.Printf("%s  %v\n", header, err)

		return
	}

	set, err := candidatesFor(r, p, branch, filter)
	if err != nil {
		fmt.Printf("%s  %v\n", header, err)

		return
	}

	printCandidates(p, branch, set, filter, false)
}

func cherryPick(rctx *run.Ctx, p *config.Project, branch string, shas []string) error {
	r := repoOf(p)
	if err := requireClean(r); err != nil {
		return err
	}

	if err := checkoutTracking(r, branch); err != nil {
		return err
	}

	for _, sha := range shas {
		if err := pickOne(rctx, r, p, branch, sha); err != nil {
			return err
		}
	}

	// The picks are already committed locally; the review happens before they
	// leave the machine, since a resolved submodule pin is only visible now.
	ok, err := reviewUnpushed(rctx, r, p.Name, branch)
	if err != nil {
		return err
	}

	if !ok {
		fmt.Printf("    %s, the picks stay on local %s in %s\n", run.Yellow("declined"), branch, r.Dir)
		fmt.Println(run.Dim(fmt.Sprintf("      git -C %s log -p %s origin/%s..%s   # inspect",
			r.Dir, submoduleLog, branch, branch)))
		fmt.Println(run.Dim(fmt.Sprintf("      git -C %s reset --hard origin/%s  # discard", r.Dir, branch)))

		return errDeclined
	}

	if err := remoteGit(rctx, r, "push", "--no-follow-tags", "origin", branch); err != nil {
		return err
	}

	sha, err := r.Git("rev-parse", "HEAD")
	if err != nil {
		return err
	}

	return watchCI(rctx, r, p, sha, branch, "")
}

func pickOne(rctx *run.Ctx, r gitx.Repo, p *config.Project, branch, sha string) error {
	short := shorten(sha, shortSHALen)

	out, err := r.Git("cherry-pick", "-x", sha)
	if err == nil {
		fmt.Printf("    picked %s\n", planHash(short))

		return nil
	}

	if isEmptyPick(out) {
		fmt.Printf("    %s %s is already in %s, skipping\n", run.Warn(), short, branch)

		_, skipErr := r.Git("cherry-pick", "--skip")

		return skipErr
	}

	if err := resolveConflicts(rctx, r, p, short, err); err != nil {
		return err
	}

	cont, contErr := r.GitEnv(noEditorEnv(), "cherry-pick", "--continue")
	if contErr != nil {
		if isEmptyPick(cont) {
			fmt.Printf("    %s %s became empty after resolving, skipping\n", run.Warn(), short)

			_, skipErr := r.Git("cherry-pick", "--skip")

			return skipErr
		}

		return fmt.Errorf("cherry-pick --continue failed for %s: %w", short, contErr)
	}

	fmt.Printf("    picked %s (submodule pins resolved from the release freeze)\n", planHash(short))

	return nil
}

func isEmptyPick(out string) bool {
	return strings.Contains(out, "previous cherry-pick is now empty") ||
		strings.Contains(out, "nothing to commit")
}

// resolveConflicts keeps the release freeze: the .gitmodules of the release
// branch wins and every conflicting submodule is re-pinned to the head of the
// branch it tracks there. A conflict anywhere else is left for a human, and a
// failure that is not a conflict is reported as git described it.
func resolveConflicts(rctx *run.Ctx, r gitx.Repo, p *config.Project, short string, cause error) error {
	conflicted, err := r.Lines("diff", "--name-only", "--diff-filter=U")
	if err != nil {
		return fmt.Errorf("cherry-pick of %s failed and the conflicts could not be read: %w", short, err)
	}

	if len(conflicted) == 0 {
		// Nothing is conflicted, so the pick failed for another reason: an
		// unknown revision, a dirty tree, a pick already in progress. Report
		// what git said instead of inventing a conflict.
		return fmt.Errorf("cherry-pick of %s failed in %s: %w", short, p.Name, cause)
	}

	rest, err := keepReleaseGitmodules(r, conflicted)
	if err != nil {
		return err
	}

	manual, err := repinSubmodules(rctx, r, p, rest)
	if err != nil {
		return err
	}

	if len(manual) > 0 {
		return manualStop(r, p, short, manual)
	}

	return nil
}

// keepReleaseGitmodules resolves a conflicting .gitmodules in favour of the
// release branch and returns the remaining conflicting paths. It runs first so
// the submodule branches below are read from the frozen version.
func keepReleaseGitmodules(r gitx.Repo, conflicted []string) ([]string, error) {
	rest := make([]string, 0, len(conflicted))

	for _, path := range conflicted {
		if path != ".gitmodules" {
			rest = append(rest, path)

			continue
		}

		if _, err := r.Git("checkout", "--ours", "--", ".gitmodules"); err != nil {
			return nil, err
		}

		if _, err := r.Git("add", "--", ".gitmodules"); err != nil {
			return nil, err
		}

		fmt.Println("    .gitmodules: kept the release version")
	}

	return rest, nil
}

// repinSubmodules resolves conflicting submodule pins from the release freeze
// and returns the paths that need a human.
func repinSubmodules(rctx *run.Ctx, r gitx.Repo, p *config.Project, paths []string) ([]string, error) {
	smPaths, err := r.SubmodulePaths()
	if err != nil {
		return nil, err
	}

	isSubmodule := make(map[string]bool, len(smPaths))
	for _, sp := range smPaths {
		isSubmodule[sp] = true
	}

	var manual []string

	for _, path := range paths {
		if !isSubmodule[path] {
			manual = append(manual, path)

			continue
		}

		if err := repinSubmodule(rctx, r, p, path); err != nil {
			return nil, err
		}
	}

	return manual, nil
}

func repinSubmodule(rctx *run.Ctx, r gitx.Repo, p *config.Project, path string) error {
	branch, err := r.SubmoduleBranch(path)
	if err != nil {
		return err
	}

	if branch == "" {
		return fmt.Errorf("submodule %s has no branch in .gitmodules of %s: "+
			"run deps freeze first, otherwise --remote would follow the default branch", path, p.Name)
	}

	if err := updateSubmoduleRemote(rctx, p, r, path, branch); err != nil {
		return err
	}

	if _, err := r.Git("add", "--", path); err != nil {
		return err
	}

	fmt.Printf("    submodule %s: dev pin ignored, re-pinned to head of %s\n", path, planRef(branch))

	return nil
}

func manualStop(r gitx.Repo, p *config.Project, short string, paths []string) error {
	fmt.Printf("\ncherry-pick of %s conflicts in %s\n", short, p.Name)

	if len(paths) > 0 {
		fmt.Printf("conflicting paths: %s\n", strings.Join(paths, ", "))
	}

	fmt.Print("the cherry-pick is left in progress, resolve it by hand:\n")
	fmt.Println(run.Dim("  cd " + r.Dir))
	fmt.Println(run.Dim("  git status"))
	fmt.Println(run.Dim("  git cherry-pick --continue   # or --abort"))
	fmt.Println()

	return fmt.Errorf("manual conflict resolution required in %s", p.Name)
}
