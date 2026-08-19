package cli

import (
	"testing"

	"github.com/grinderz/repo-tools/internal/config"
	"github.com/grinderz/repo-tools/internal/gitx"
)

// A value with a literal | must not break the table it sits in.
func TestMarkdownRow(t *testing.T) {
	if got := markdownRow([]string{"a", "b|c"}); got != `| a | b\|c |` {
		t.Errorf("row = %q", got)
	}

	if got := markdownRule(3); got != "|---|---|---|" {
		t.Errorf("rule = %q", got)
	}
}

// Missing facts render as a dash: a table cell is no place for an error.
func TestReportVarsWithoutAClone(t *testing.T) {
	p := &config.Project{Name: "ghost", DevBranch: "develop", ProjectDir: t.TempDir()}

	vars := reportVars(testCtx(), p)
	if vars[varCommit] != "-" || vars[varTag] != "-" || vars[varSubject] != "-" {
		t.Errorf("vars = %v", vars)
	}

	if vars[varProject] != "ghost" || vars[varBranch] != "develop" {
		t.Errorf("the message placeholders must still be there: %v", vars)
	}
}

// The dev config reads the dev branch head; a tag on the repository shows up
// as the overall highest, since develop is not a release line.
func TestReportVarsOnTheDevBranch(t *testing.T) {
	f := newFixture(t)
	p := f.project(t)
	r := gitx.Repo{Dir: f.parent}

	if _, err := r.Git("tag", "1.2.0"); err != nil {
		t.Fatal(err)
	}

	if _, err := r.Git("tag", "1.2.1-rc.3"); err != nil {
		t.Fatal(err)
	}

	vars := reportVars(testCtx(), p)
	if vars[varCommit] == "-" || vars[varSubject] == "-" {
		t.Errorf("the dev head must be reported: %v", vars)
	}

	if vars[varTag] != "1.2.1-rc.3" {
		t.Errorf("tag = %q", vars[varTag])
	}
}

// A release config names a release branch, so {tag} is that line's highest —
// not a newer tag from another line.
func TestReportVarsOnAReleaseBranch(t *testing.T) {
	f := newFixture(t)
	p := f.project(t)
	p.ReleaseBranch = "release-1.0"
	r := gitx.Repo{Dir: f.parent}

	for _, tag := range []string{"1.0.0", "1.0.1-rc.0", "2.0.0"} {
		if _, err := r.Git("tag", tag); err != nil {
			t.Fatal(err)
		}
	}

	vars := reportVars(testCtx(), p)
	if vars[varTag] != "1.0.1-rc.0" {
		t.Errorf("tag = %q, want the release line's own highest", vars[varTag])
	}
}

// Without a report list the table still says something useful.
func TestReportColumnsDefault(t *testing.T) {
	columns := reportColumns(&config.Config{})
	if len(columns) != 3 || columns[0].Column != "Project" {
		t.Errorf("columns = %v", columns)
	}

	own := &config.Config{Report: []config.ReportColumn{{Column: "X", Value: "{tag}"}}}
	if columns := reportColumns(own); len(columns) != 1 || columns[0].Column != "X" {
		t.Errorf("the config's own table must win: %v", columns)
	}
}

// {tag|branch} renders the first fact that exists: a line not tagged yet
// says which branch ships instead of a bare dash.
func TestExpandReportFallbacks(t *testing.T) {
	vars := map[string]string{"tag": "-", "branch": "release-1.4", "commit": ""}

	if got := expandReport("{tag|branch}", vars); got != "release-1.4" {
		t.Errorf("got %q", got)
	}

	if got := expandReport("{commit|tag}", vars); got != "-" {
		t.Errorf("nothing real in the chain: %q", got)
	}

	vars["tag"] = "1.4.0-rc.1"

	if got := expandReport("{tag|branch}", vars); got != "1.4.0-rc.1" {
		t.Errorf("the first real value wins: %q", got)
	}

	// A single placeholder is the ordinary expansion, dash and all.
	if got := expandReport("{commit}", vars); got != "" {
		t.Errorf("got %q", got)
	}
}
