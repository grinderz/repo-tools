// Package changelog generates the plain git-log changelog, the one the shared
// changelog-gen.sh script used to produce: commits grouped by commit date,
// newest day first.
package changelog

import (
	"sort"
	"strings"
	"time"

	"github.com/grinderz/repo-tools/internal/gitx"
)

// Defaults matching the script this replaces.
const (
	DefaultSince  = "2000-01-01"
	DefaultHeader = "# CHANGELOG"

	dateLayout = "2006-01-02"
	// The script indents every entry by one space; git output is captured
	// trimmed, so the indent is added back per line instead of by the format.
	logFormat = "* %s (%an)"
	logIndent = " "
	// fieldSep cannot appear in a subject, unlike a tab.
	fieldSep = "\x1f"
)

// Options controls the generated document.
type Options struct {
	Header string // first line, without a trailing newline
	Since  string // YYYY-MM-DD, inclusive
	Until  string // YYYY-MM-DD, inclusive
}

func (o Options) withDefaults() Options {
	if o.Header == "" {
		o.Header = DefaultHeader
	}

	if o.Since == "" {
		o.Since = DefaultSince
	}

	if o.Until == "" {
		o.Until = time.Now().Format(dateLayout)
	}

	return o
}

// GitLog renders the changelog of a repository.
func GitLog(r gitx.Repo, opts Options) (string, error) {
	opts = opts.withDefaults()

	days, order, err := commitsByDate(r, opts)
	if err != nil {
		return "", err
	}

	var doc strings.Builder

	doc.WriteString(opts.Header + "\n")

	for _, date := range order {
		doc.WriteString("\n## [" + date + "]\n\n")
		doc.WriteString(logIndent + strings.Join(days[date], "\n"+logIndent) + "\n")
	}

	return doc.String(), nil
}

// commitsByDate reads the whole window in one git call and groups the commits
// by commit date: oldest first inside a day, days newest first. One call keeps
// this linear — asking git per day cost a subprocess for every date in history.
//
// Dates come from %cd, so a commit is filed under the date its author saw. The
// shell script this replaces took its dates from %cd as well but then re-queried
// each day with --since/--until, which git reads in the local timezone: commits
// made near midnight in another timezone landed under a neighbouring date, or
// vanished from the document when that neighbour had no commits of its own.
func commitsByDate(r gitx.Repo, opts Options) (map[string][]string, []string, error) {
	// git log fails outright on a repository without commits.
	if !r.HasCommits() {
		return nil, nil, nil
	}

	lines, err := r.Lines(
		"log", "--no-merges", "--no-color", "--reverse",
		"--since="+opts.Since, "--until="+opts.Until,
		"--format=%cd"+fieldSep+logFormat, "--date=short",
	)
	if err != nil {
		return nil, nil, err
	}

	days := make(map[string][]string, len(lines))
	order := make([]string, 0, len(lines))

	for _, line := range lines {
		date, entry, ok := strings.Cut(line, fieldSep)
		if !ok {
			continue
		}

		if _, seen := days[date]; !seen {
			order = append(order, date)
		}

		days[date] = append(days[date], entry)
	}

	sort.Sort(sort.Reverse(sort.StringSlice(order)))

	return days, order, nil
}
