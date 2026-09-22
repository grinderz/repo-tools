package cli

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/grinderz/repo-tools/internal/config"
	"github.com/grinderz/repo-tools/internal/gitx"
	"github.com/grinderz/repo-tools/internal/run"
)

// fieldSep separates the fields of one git log line; \x1f cannot appear in a
// subject, unlike a tab. logFields is how many the format below produces.
const (
	fieldSep  = "\x1f"
	logFields = 4
)

// candidate is one dev-branch commit that the release branch does not have.
type candidate struct {
	SHA     string
	Short   string
	Author  string
	Subject string
}

func (c candidate) String() string {
	return fmt.Sprintf("%s\t%s\t%s", planHash(c.Short), run.Dim(c.Author), c.Subject)
}

// pickFilter narrows the candidates. An empty filter keeps everything.
//
// Subject terms (--task, --grep) are a selection: what they match is what gets
// picked. Dates (--since, --until) only scope the list, so they still leave the
// choice to the operator.
type pickFilter struct {
	Tasks []string         // ticket ids as given, kept for the description
	terms []*regexp.Regexp // what a subject is actually matched against
	Greps []string         // patterns as given, kept for the description
	Since string           // any date git understands, commit date
	Until string
}

func newPickFilter(tasks, greps []string, since, until string) (pickFilter, error) {
	f := pickFilter{Tasks: tasks, Greps: greps, Since: since, Until: until}

	for _, task := range tasks {
		// Whole-id match: AB-315 must not also select AB-3151.
		f.terms = append(f.terms, regexp.MustCompile(`(?i)\b`+regexp.QuoteMeta(task)+`\b`))
	}

	for _, grep := range greps {
		re, err := regexp.Compile("(?i)" + grep)
		if err != nil {
			return pickFilter{}, fmt.Errorf("--grep %q: %w", grep, err)
		}

		f.terms = append(f.terms, re)
	}

	return f, nil
}

// selects reports whether the filter names the commits to pick by itself.
func (f pickFilter) selects() bool { return len(f.terms) > 0 }

// empty reports whether the filter narrows nothing at all.
func (f pickFilter) empty() bool { return !f.selects() && f.Since == "" && f.Until == "" }

// matches reports whether a subject satisfies any of the filter's terms.
// Dates are not checked here: git log applies them.
func (f pickFilter) matches(subject string) bool {
	if !f.selects() {
		return true
	}

	for _, re := range f.terms {
		if re.MatchString(subject) {
			return true
		}
	}

	return false
}

func (f pickFilter) describe() string {
	terms := make([]string, 0, len(f.Tasks)+len(f.Greps)+2) //nolint:mnd // room for both dates
	terms = append(terms, f.Tasks...)
	terms = append(terms, f.Greps...)

	if f.Since != "" {
		terms = append(terms, "since "+f.Since)
	}

	if f.Until != "" {
		terms = append(terms, "until "+f.Until)
	}

	return strings.Join(terms, ", ")
}

// candidateSet is what a filter selected, plus what the release branch already
// has, so an empty selection can say which of the two it is.
type candidateSet struct {
	Picking       []candidate // match the filter and are missing from the release branch
	AlreadyInSync int         // match the filter but are already there
}

// candidatesFor lists, oldest first, the dev-branch commits the release branch
// is missing, keeping only those the filter accepts. Commits the filter accepts
// but the release branch already carries are counted, not returned.
func candidatesFor(r gitx.Repo, p *config.Project, branch string, filter pickFilter) (candidateSet, error) {
	// A config may name a release branch before anyone creates it; that is a
	// state to report, not a raw "unknown commit" from git.
	if !r.RemoteBranchExists(branch) {
		return candidateSet{}, pendingRelease(branch)
	}

	missing, err := candidates(r, p, branch)
	if err != nil {
		return candidateSet{}, err
	}

	isMissing := make(map[string]bool, len(missing))
	for _, sha := range missing {
		isMissing[sha] = true
	}

	cands, err := rangeLog(r, "origin/"+branch+".."+"origin/"+p.DevBranch, filter)
	if err != nil {
		return candidateSet{}, err
	}

	set := candidateSet{Picking: make([]candidate, 0, len(missing))}

	for _, cand := range cands {
		if !filter.matches(cand.Subject) {
			continue
		}

		if isMissing[cand.SHA] {
			set.Picking = append(set.Picking, cand)
		} else {
			set.AlreadyInSync++
		}
	}

	return set, nil
}

// rangeLog lists the commits of one range, oldest first — the order picks have
// to run in — one candidate per commit. The filter's date window, if any, is
// git's job; the subject terms are the caller's, since only it knows whether a
// non-match is dropped or just counted.
func rangeLog(r gitx.Repo, rng string, filter pickFilter) ([]candidate, error) {
	args := []string{
		"log", "--no-merges", "--reverse", "--no-color",
		"--format=%H" + fieldSep + "%h" + fieldSep + "%an" + fieldSep + "%s",
	}

	if filter.Since != "" {
		args = append(args, "--since="+filter.Since)
	}

	if filter.Until != "" {
		args = append(args, "--until="+filter.Until)
	}

	lines, err := r.Lines(append(args, rng)...)
	if err != nil {
		return nil, err
	}

	out := make([]candidate, 0, len(lines))

	for _, line := range lines {
		parts := strings.SplitN(line, fieldSep, logFields)
		if len(parts) != logFields {
			continue
		}

		out = append(out, candidate{SHA: parts[0], Short: parts[1], Author: parts[2], Subject: parts[3]})
	}

	return out, nil
}

func shasOf(cands []candidate) []string {
	out := make([]string, 0, len(cands))
	for _, c := range cands {
		out = append(out, c.SHA)
	}

	return out
}
