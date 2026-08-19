package cli

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/spf13/cobra"

	"github.com/grinderz/repo-tools/internal/config"
	"github.com/grinderz/repo-tools/internal/gitx"
	"github.com/grinderz/repo-tools/internal/run"
)

// Placeholders only the report knows, next to the usual message ones.
const (
	varCommit  = "commit"
	varSubject = "subject"
	varTag     = "tag"
)

// newReportCmd prints one markdown table over the selected projects — the
// summary a release announcement or a status page wants, ready to paste. The
// columns come from the config, so the dev config reports commits and a
// release config reports tags with the same command.
func newReportCmd(rctx *run.Ctx, name string) *cobra.Command {
	return &cobra.Command{
		Use:   name + " [project...]",
		Short: "Print a markdown table of the projects",
		Long: "Prints one markdown table row per project, ready to paste. The columns\n" +
			"are the config's report list: a header and a value template each. The\n" +
			"templates take {project}, {branch}, {product_version}, and per project\n" +
			"{commit} (short head of the target branch), {subject} (its commit\n" +
			"subject) and {tag} (the highest tag of the branch's release line, or of\n" +
			"the repository when the branch is not a release one). Without a report\n" +
			"list the table is project | branch | commit.",
		RunE: func(_ *cobra.Command, args []string) error {
			projects, err := rctx.Select(args)
			if err != nil {
				return err
			}

			columns := reportColumns(rctx.Cfg)

			headers := make([]string, 0, len(columns))
			for _, col := range columns {
				headers = append(headers, col.Column)
			}

			fmt.Println(markdownRow(headers))
			fmt.Println(markdownRule(len(columns)))

			for _, p := range projects {
				vars := reportVars(rctx, p)

				cells := make([]string, 0, len(columns))
				for _, col := range columns {
					cells = append(cells, expandReport(col.Value, vars))
				}

				fmt.Println(markdownRow(cells))
			}

			return nil
		},
	}
}

// reportPlaceholder is a placeholder with fallbacks: {tag|branch} and longer
// chains. A single {tag} goes through the ordinary expansion.
var reportPlaceholder = regexp.MustCompile(`\{([a-zA-Z_]+(?:\|[a-zA-Z_]+)+)\}`)

// expandReport is expand plus fallbacks: {tag|branch} renders the first name
// in the chain that has a real value — "-" is the missing-fact marker, not a
// value — and "-" itself when none does. A release line that is not tagged
// yet can then say which branch ships instead of a bare dash.
func expandReport(tpl string, vars map[string]string) string {
	tpl = reportPlaceholder.ReplaceAllStringFunc(tpl, func(match string) string {
		for name := range strings.SplitSeq(match[1:len(match)-1], "|") {
			if value, ok := vars[name]; ok && value != "" && value != "-" {
				return value
			}
		}

		return "-"
	})

	return expand(tpl, vars)
}

// reportColumns is the config's table, or the built-in one.
func reportColumns(cfg *config.Config) []config.ReportColumn {
	if len(cfg.Report) > 0 {
		return cfg.Report
	}

	return []config.ReportColumn{
		{Column: "Project", Value: "{project}"},
		{Column: "Branch", Value: "{branch}"},
		{Column: "Commit", Value: "`{commit}`"},
	}
}

// reportVars is the message placeholders plus what only a summary asks for:
// the head commit, its subject and the highest tag. A fact that is not there
// — no clone, no branch on origin, no tag yet — renders as a dash, because a
// table cell is no place for an error message.
func reportVars(rctx *run.Ctx, p *config.Project) map[string]string {
	branch := p.TargetBranch()
	vars := messageVars(rctx, p, branch)

	vars[varCommit], vars[varSubject], vars[varTag] = "-", "-", "-"

	r := repoOf(p)
	if !r.IsRepo() {
		return vars
	}

	// The report is read-only and meant to be quick, so --fetch is opt-in.
	if rctx.WantFetchFor(p, false) {
		if reason := fetchOrWarn(rctx, p, r); reason != "" {
			fmt.Fprintln(os.Stderr, p.Name+": "+reason)
		}
	}

	if ref := branchRef(r, branch); ref != "" {
		if short, err := r.Git("rev-parse", "--short", ref); err == nil {
			vars[varCommit] = short
		}

		if subject, err := r.Git("log", "-1", "--format=%s", ref); err == nil {
			vars[varSubject] = subject
		}
	}

	if tag, ok := highestTag(r, p, branch); ok {
		vars[varTag] = tag
	}

	return vars
}

// highestTag is the branch's release line when it is a release branch, the
// repository's highest tag otherwise.
func highestTag(r gitx.Repo, p *config.Project, branch string) (string, bool) {
	tags, err := r.Tags()
	if err != nil || len(tags) == 0 {
		return "", false
	}

	if rel, ok := gitx.ParseReleaseBranch(branch, p.ReleaseBranchPrefix); ok {
		if line := gitx.TagsFor(tags, rel); len(line) > 0 {
			return line[len(line)-1].String(), true
		}

		return "", false
	}

	if best, ok := gitx.LatestTag(tags); ok {
		return best.String(), true
	}

	return "", false
}

// markdownRow renders one table row, keeping a literal | in a value from
// breaking the table apart.
func markdownRow(cells []string) string {
	escaped := make([]string, 0, len(cells))
	for _, cell := range cells {
		escaped = append(escaped, strings.ReplaceAll(cell, "|", "\\|"))
	}

	return "| " + strings.Join(escaped, " | ") + " |"
}

// markdownRule is the header separator of a table with n columns.
func markdownRule(n int) string {
	return "|" + strings.Repeat("---|", n)
}
