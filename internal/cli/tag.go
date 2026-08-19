package cli

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/grinderz/repo-tools/internal/config"
	"github.com/grinderz/repo-tools/internal/gitx"
	"github.com/grinderz/repo-tools/internal/run"
)

// tagKind picks between release candidate and final release tagging.
type tagKind int

const (
	tagRC tagKind = iota
	tagFinal
)

func newRcCmd(rctx *run.Ctx, name string) *cobra.Command {
	return newTagCmd(rctx, name, "Tag release candidates on the release branches and push", tagRC)
}

func newReleaseCmd(rctx *run.Ctx, name string) *cobra.Command {
	return newTagCmd(rctx, name, "Tag final releases on the release branches and push", tagFinal)
}

func newTagCmd(rctx *run.Ctx, name, short string, kind tagKind) *cobra.Command {
	var releaseBranch, tagOverride string

	c := &cobra.Command{
		Use:   name + " [project...]",
		Short: short,
		RunE: func(cmd *cobra.Command, args []string) error {
			projects, err := rctx.Select(args)
			if err != nil {
				return err
			}

			steps := make([]run.Step, 0, len(projects))

			for _, p := range projects {
				st, err := planTag(rctx, p, kind, releaseBranch, tagOverride)
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
		&releaseBranch,
		"release-branch",
		"",
		"release branch to tag (default: the highest release branch on origin)",
	)
	c.Flags().StringVar(&tagOverride, "tag", "", "explicit tag to create instead of the computed one")

	return c
}

// tagTarget is the branch to tag and its version, or the reason there is
// none. A reason is a state of the project the plan reports as a skip; a
// failed fetch is neither — it is an error, or a release flow would sail past
// its tagging step on a hiccup nobody saw.
func tagTarget(
	rctx *run.Ctx,
	r gitx.Repo,
	p *config.Project,
	releaseBranch string,
) (string, gitx.ReleaseVer, string, error) {
	if !p.ReleaseTagsEnabled() {
		return "", gitx.ReleaseVer{}, "release tags are disabled for this project", nil
	}

	if reason := missingRepo(r); reason != "" {
		return "", gitx.ReleaseVer{}, reason, nil
	}

	if err := fetch(rctx, p, r); err != nil {
		return "", gitx.ReleaseVer{}, "", fetchFailed(err)
	}

	branch, ver, err := latestRelease(r, p, releaseBranch)
	if err != nil {
		return "", gitx.ReleaseVer{}, err.Error(), nil //nolint:nilerr // the error becomes the skip reason, not a failure
	}

	// Tagging works on origin/<branch>: without it there is no commit to tag,
	// no history to list in the message, and git would say so in its own words.
	if !r.RemoteBranchExists(branch) {
		return "", gitx.ReleaseVer{}, pendingReleaseWarn(branch), nil
	}

	return branch, ver, "", nil
}

func planTag(
	rctx *run.Ctx,
	p *config.Project,
	kind tagKind,
	releaseBranch, tagOverride string,
) (run.Step, error) {
	r := repoOf(p)
	st := run.Step{Project: p}

	branch, ver, reason, err := tagTarget(rctx, r, p, releaseBranch)
	if err != nil {
		return st, err
	}

	if reason != "" {
		st.Skip, st.Warn = true, reason

		return st, nil
	}

	// Rerunning a flow over an unchanged branch must not mint a new tag for
	// the same commit: the tag that already marks the head is the answer.
	if existing := alreadyTagged(r, ver, kind, branch); existing != "" {
		st.Skip, st.Warn = true, fmt.Sprintf("%s already tags the head of origin/%s", existing, branch)

		return st, nil
	}

	tag, err := tagFor(r, ver, kind, tagOverride)
	if err != nil {
		st.Skip, st.Warn = true, err.Error()

		return st, nil //nolint:nilerr // the error becomes the skip reason, not a failure
	}

	if tagExists(r, tag) {
		st.Skip, st.Warn = true, fmt.Sprintf("tag %s already exists", tag)

		return st, nil
	}

	msgTpl := rctx.Cfg.RcTagMessage
	if kind == tagFinal {
		msgTpl = rctx.Cfg.ReleaseTagMessage
	}

	vars := messageVars(rctx, p, branch)
	vars[varVersion] = tag

	if err := addCommitVars(vars, msgTpl, r, p, branch, ver); err != nil {
		st.Skip, st.Warn = true, err.Error()

		return st, nil //nolint:nilerr // the error becomes the skip reason, not a failure
	}

	msg := expand(msgTpl, vars)
	plan := fmt.Sprintf("tag %s on %s (%s) and push",
		planRef(tag), planRef("origin/"+branch), planMsg(summarize(msg)))
	if rctx.ShowDiff() {
		plan += " (message shown first)"
	}

	plan += ciPlanSuffix(rctx, p)

	st.Plan = []string{plan}
	st.Warn = tagWarnings(rctx, r, p, kind, branch, tag)

	warn := st.Warn
	st.Exec = func() error { return createTag(rctx, p, branch, tag, msg, warn) }

	return st, nil
}

// tagWarnings collects what makes a tag questionable rather than impossible:
// a final release the release candidates no longer describe, and submodules
// that were never frozen. Both are reasons to look before answering yes.
func tagWarnings(rctx *run.Ctx, r gitx.Repo, p *config.Project, kind tagKind, branch, tag string) string {
	var warnings []string

	if kind == tagFinal {
		if w := staleRcWarning(r, tag, branch); w != "" {
			warnings = append(warnings, w)
		}
	}

	if stale := unfrozenSubmodules(rctx, r, p, branchRef(r, branch)); len(stale) > 0 {
		warnings = append(warnings, fmt.Sprintf("submodules differ from freeze_to (%s), run deps freeze first",
			strings.Join(stale, "; ")))
	}

	return strings.Join(warnings, "; ")
}

// staleRcWarning reports a final tag whose release candidates are behind the
// branch head, or a final release nobody ever built a candidate for.
func staleRcWarning(r gitx.Repo, tag, branch string) string {
	final, ok := gitx.ParseTag(tag)
	if !ok {
		return ""
	}

	tags, err := r.Tags()
	if err != nil {
		return ""
	}

	last := ""

	for _, t := range gitx.TagsFor(tags, gitx.ReleaseVer{Major: final.Major, Minor: final.Minor}) {
		if t.Patch == final.Patch && t.RC >= 0 {
			last = t.String()
		}
	}

	if last == "" {
		return "no release candidate was tagged for " + tag
	}

	count, err := r.Git("rev-list", "--count", "--no-merges", last+"..origin/"+branch)
	if err != nil || count == "0" {
		return ""
	}

	return fmt.Sprintf("%s has moved %s commit(s) since %s, which is what was tested", branch, count, last)
}

// unfrozenSubmodules lists the configured submodules whose .gitmodules branch
// on the release branch is not the one deps freeze would set. Either way round
// it means the tag pins something other than what the config describes.
func unfrozenSubmodules(rctx *run.Ctx, r gitx.Repo, p *config.Project, ref string) []string {
	if !p.DepsFreezeEnabled() || ref == "" {
		return nil
	}

	var stale []string

	submodules, err := freezeList(rctx, r, p, ref)
	if err != nil {
		return nil
	}

	for _, sm := range submodules {
		want, err := resolveFreezeBranch(rctx, r, p, sm, ref)
		if err != nil {
			continue
		}

		got, err := r.SubmoduleBranchAt(ref, sm.Path)
		if err != nil || got == want {
			continue
		}

		stale = append(stale, fmt.Sprintf("%s tracks %s, config says %s", sm.Path, orNone(got), want))
	}

	return stale
}

func orNone(branch string) string {
	if branch == "" {
		return "no branch"
	}

	return branch
}

// summarize renders a tag message for the plan: a commit list makes it many
// lines long, and the plan is one line per step.
func summarize(msg string) string {
	first, rest, found := strings.Cut(msg, "\n")
	if !found {
		return fmt.Sprintf("%q", msg)
	}

	return fmt.Sprintf("%q +%d more line(s)", first, strings.Count(rest, "\n")+1)
}

// addCommitVars fills in the placeholders describing what the tag contains,
// and only then reads the history: a template that does not ask for a commit
// list should not pay for one.
func addCommitVars(
	vars map[string]string,
	tpl string,
	r gitx.Repo,
	p *config.Project,
	branch string,
	ver gitx.ReleaseVer,
) error {
	wanted := false

	for _, name := range []string{varCommits, varCommitHashes, varCommitCount} {
		if strings.Contains(tpl, "{"+name+"}") {
			wanted = true
		}

		vars[name] = ""
	}

	if !wanted {
		return nil
	}

	commits, err := tagCommits(r, p, branch, ver)
	if err != nil {
		return err
	}

	hashes := make([]string, 0, len(commits))
	lines := make([]string, 0, len(commits))

	for _, c := range commits {
		hash, subject, _ := strings.Cut(c, fieldSep)
		hashes = append(hashes, hash)
		lines = append(lines, hash+" "+subject)
	}

	vars[varCommits] = strings.Join(lines, "\n")
	vars[varCommitHashes] = strings.Join(hashes, "\n")
	vars[varCommitCount] = strconv.Itoa(len(commits))

	return nil
}

// tagCommits lists what the tag adds: everything since the previous tag of the
// same release branch, or — for the first tag there — the commits the release
// branch has and the dev branch does not, which is exactly what was
// cherry-picked into it.
// tagRange is what the tag message lists. Within a release line that is the
// work since the line's previous tag. The first tag on a freshly cut branch has
// no such tag, and the branch carries nothing the dev branch does not, so it
// counts from the previous release line instead — that is the release. Only a
// project with no tags at all falls back to what the branch has and the dev
// branch does not, which is the cherry-picks.
func tagRange(tags []string, p *config.Project, branch string, ver gitx.ReleaseVer) string {
	head := "..origin/" + branch

	if existing := gitx.TagsFor(tags, ver); len(existing) > 0 {
		return existing[len(existing)-1].String() + head
	}

	if previous, ok := gitx.PreviousTag(tags, ver); ok {
		return previous.String() + head
	}

	return "origin/" + p.DevBranch + head
}

func tagCommits(r gitx.Repo, p *config.Project, branch string, ver gitx.ReleaseVer) ([]string, error) {
	tags, err := r.Tags()
	if err != nil {
		return nil, err
	}

	return r.Lines("log", "--no-merges", "--reverse", "--format=%h"+fieldSep+"%s",
		tagRange(tags, p, branch, ver))
}

func tagFor(r gitx.Repo, ver gitx.ReleaseVer, kind tagKind, override string) (string, error) {
	if override != "" {
		return override, nil
	}

	tags, err := r.Tags()
	if err != nil {
		return "", err
	}

	if kind == tagRC {
		return gitx.NextRc(tags, ver).String(), nil
	}

	return gitx.NextFinal(tags, ver).String(), nil
}

// alreadyTagged is the tag of this release line that already marks the head
// of origin/branch, empty when none does. Any tag blocks another rc — a
// second candidate for the same commit says nothing new — but an rc does not
// block the final that promotes it.
func alreadyTagged(r gitx.Repo, ver gitx.ReleaseVer, kind tagKind, branch string) string {
	tags, err := r.Tags()
	if err != nil {
		return ""
	}

	head, err := r.Git("rev-parse", "origin/"+branch)
	if err != nil {
		return ""
	}

	for _, existing := range gitx.TagsFor(tags, ver) {
		if kind == tagFinal && existing.RC >= 0 {
			continue // an rc is what the final promotes, not a reason to skip
		}

		target, err := r.Git("rev-parse", existing.String()+"^{commit}")
		if err != nil || target != head {
			continue
		}

		return existing.String()
	}

	return ""
}

func tagExists(r gitx.Repo, tag string) bool {
	_, err := r.Git("rev-parse", "--verify", "--quiet", "refs/tags/"+tag)

	return err == nil
}

func createTag(rctx *run.Ctx, p *config.Project, branch, tag, msg, warn string) error {
	r := repoOf(p)
	if !r.RemoteBranchExists(branch) {
		return fmt.Errorf("origin/%s does not exist", branch)
	}

	ok, err := reviewTag(rctx, r, p.Name, tag, branch, msg, warn)
	if err != nil {
		return err
	}

	if !ok {
		fmt.Printf("    %s, %s was not created in %s\n", run.Yellow("declined"), tag, r.Dir)

		return errDeclined
	}

	if _, err := r.Git("tag", "-a", tag, "-m", msg, "origin/"+branch); err != nil {
		return err
	}

	if err := remoteGit(rctx, r, "push", "origin", "refs/tags/"+tag); err != nil {
		// Drop the local tag again: leaving it behind would make the next run
		// report "tag already exists" and skip, hiding this failure.
		if _, delErr := r.Git("tag", "-d", tag); delErr != nil {
			return fmt.Errorf("%w (and the local tag could not be removed: %w)", err, delErr)
		}

		return err
	}

	// The tag pipeline runs on the tagged commit, under the tag's own ref.
	sha, err := r.Git("rev-parse", "origin/"+branch)
	if err != nil {
		return err
	}

	return watchCI(rctx, r, p, sha, tag, "")
}
