package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/grinderz/repo-tools/internal/config"
	"github.com/grinderz/repo-tools/internal/gitx"
	"github.com/grinderz/repo-tools/internal/run"
)

// newReleaseNotesCmd prints the whole release as one markdown document: per
// project, the commits its latest tag added over the previous one — the
// announcement text, ready to paste.
func newReleaseNotesCmd(rctx *run.Ctx, name string) *cobra.Command {
	var releaseBranch string

	c := &cobra.Command{
		Use:   name + " [project...]",
		Short: "Print markdown release notes across the projects",
		Long: "One markdown section per project: the highest tag of its release line\n" +
			"and the commits it added since the line's previous tag (for the first\n" +
			"tag of a line, since the previous release line — which is the release).\n" +
			"Everything goes to stdout with no colour, ready to paste; states like a\n" +
			"missing clone or an untagged line become italic notes, not errors.\n\n" +
			"The report is read-only and quick, so --fetch is opt-in.",
		RunE: func(_ *cobra.Command, args []string) error {
			projects, err := rctx.Select(args)
			if err != nil {
				return err
			}

			fmt.Println(notesHeader(rctx))

			for _, p := range projects {
				fmt.Println()
				fmt.Print(notesSection(rctx, p, releaseBranch))
			}

			return nil
		},
	}
	c.Flags().StringVar(
		&releaseBranch,
		"release-branch",
		"",
		"release line to report (default: the highest release branch on origin)",
	)

	return c
}

// notesSection is one project's markdown: heading, range note, commits.
func notesSection(rctx *run.Ctx, p *config.Project, releaseBranch string) string {
	heading := "## " + p.Name + "\n\n"
	r := repoOf(p)

	if reason := missingRepo(r); reason != "" {
		return heading + "_" + reason + "_\n"
	}

	// The report is read-only and meant to be quick, so --fetch is opt-in.
	if rctx.WantFetchFor(p, false) {
		if reason := fetchOrWarn(rctx, p, r); reason != "" {
			fmt.Fprintln(os.Stderr, p.Name+": "+reason)
		}
	}

	branch, ver, err := latestRelease(r, p, releaseBranch)
	if err != nil {
		return heading + "_" + err.Error() + "_\n"
	}

	tags, err := r.Tags()
	if err != nil {
		return heading + "_" + err.Error() + "_\n"
	}

	line := gitx.TagsFor(tags, ver)
	if len(line) == 0 {
		return heading + "_no tags on the " + branch + " line yet_\n"
	}

	target := line[len(line)-1].String()
	rng, since := notesRange(tags, p, line, ver, target)

	commits, err := r.Lines("log", "--no-merges", "--reverse", "--format=- %s (`%h`)", rng)
	if err != nil {
		return heading + "_" + err.Error() + "_\n"
	}

	out := notesSectionTitle(rctx, p, branch, target) + "\n\n_" + since + "_\n\n"

	if len(commits) == 0 {
		return out + "no changes\n"
	}

	return out + strings.Join(commits, "\n") + "\n"
}

// notesHeader is the document's first line: the notes_header template, or
// the built-in heading that names the product version when there is one.
func notesHeader(rctx *run.Ctx) string {
	if tpl := rctx.Cfg.NotesHeader; tpl != "" {
		return expand(tpl, map[string]string{varProductVersion: rctx.Cfg.ProductVersion})
	}

	if rctx.Cfg.ProductVersion != "" {
		return "# Release " + rctx.Cfg.ProductVersion
	}

	return "# Release notes"
}

// notesSectionTitle is one section's heading: the notes_section_title
// template with {tag} on top of the usual placeholders, or the built-in
// "## project tag". Sections that carry a note instead of a tag keep the
// plain project heading either way.
func notesSectionTitle(rctx *run.Ctx, p *config.Project, branch, tag string) string {
	tpl := rctx.Cfg.NotesSectionTitle
	if tpl == "" {
		return "## " + p.Name + " " + tag
	}

	vars := messageVars(rctx, p, branch)
	vars[varTag] = tag

	return expand(tpl, vars)
}

// notesRange is what the latest tag added: since the line's previous tag,
// else since the previous release line (that is the release), else — for a
// repository's very first tag — what the release branch has over the dev
// branch, which is the cherry-picks.
func notesRange(
	tags []string,
	p *config.Project,
	line []gitx.TagVer,
	ver gitx.ReleaseVer,
	target string,
) (string, string) {
	if len(line) > 1 {
		prev := line[len(line)-2].String()

		return prev + ".." + target, "changes since " + prev
	}

	if prev, ok := gitx.PreviousTag(tags, ver); ok {
		return prev.String() + ".." + target, "changes since " + prev.String()
	}

	return "origin/" + p.DevBranch + ".." + target, "the first tag; what " + target + " has over " + p.DevBranch
}
