package cli

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/grinderz/repo-tools/internal/run"
)

// Renovate logs one JSON object per line (bunyan: level, msg, and whatever
// the message carries — repository, branch, pr, prTitle). The report reads
// that stream and tells it in rt's words: one ==> header per repository,
// the merge requests it opened or updated, the warnings and the errors;
// the rest of Renovate's chatter stays dim.

// Bunyan levels.
const (
	logInfo  = 30
	logWarn  = 40
	logError = 50
)

// renovateEvent is one log line, the fields the report reads.
type renovateEvent struct {
	Level        int             `json:"level"`
	Msg          string          `json:"msg"`
	Repository   string          `json:"repository"`
	Branch       string          `json:"branch"`
	BaseBranch   string          `json:"baseBranch"`
	PR           json.RawMessage `json:"pr"`
	PRTitle      string          `json:"prTitle"`
	ErrorMessage string          `json:"errorMessage"`
	Err          struct {
		Message string `json:"message"`
	} `json:"err"`
	// Errors and Warnings come as objects with a message, or as plain
	// strings, depending on who logged them.
	Errors   json.RawMessage `json:"errors"`
	Warnings json.RawMessage `json:"warnings"`
	Files    []string        `json:"files"`
	// ArtifactErrors is what a lock file update left behind: Renovate warns
	// with it once the merge request is open, the package file changed and
	// the lock file not.
	ArtifactErrors []struct {
		LockFile string `json:"lockFile"`
		Stderr   string `json:"stderr"`
	} `json:"artifactErrors"`
	Stats struct {
		Total struct {
			FileCount int `json:"fileCount"`
			DepCount  int `json:"depCount"`
		} `json:"total"`
		Managers map[string]struct {
			FileCount int `json:"fileCount"`
			DepCount  int `json:"depCount"`
		} `json:"managers"`
	} `json:"stats"`
}

// renovateIssue is one entry of a validation report.
type renovateIssue struct {
	Topic   string `json:"topic"`
	Message string `json:"message"`
}

// parseRenovateLine reads one log line; false for anything that is not a
// JSON object, which Renovate never prints but a docker pull or a crash does.
// A field shaped differently than expected costs that field, not the line:
// the level and the message are read on their own then.
func parseRenovateLine(line string) (renovateEvent, bool) {
	var event renovateEvent

	if !strings.HasPrefix(line, "{") {
		return renovateEvent{}, false
	}

	if json.Unmarshal([]byte(line), &event) == nil {
		return event, true
	}

	var minimal struct {
		Level      int    `json:"level"`
		Msg        string `json:"msg"`
		Repository string `json:"repository"`
	}

	if json.Unmarshal([]byte(line), &minimal) != nil {
		return renovateEvent{}, false
	}

	return renovateEvent{Level: minimal.Level, Msg: minimal.Msg, Repository: minimal.Repository}, true
}

// issueMessages reads an errors or warnings field: a list of strings, or of
// objects with a message.
func issueMessages(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}

	var plain []string
	if json.Unmarshal(raw, &plain) == nil {
		return plain
	}

	var objects []renovateIssue
	if json.Unmarshal(raw, &objects) != nil {
		return nil
	}

	messages := make([]string, 0, len(objects))
	for _, issue := range objects {
		messages = append(messages, issue.Message)
	}

	return messages
}

// issuePreview caps a list of issues: a lookup failure lists every package,
// and seventy lines say no more than three and a count.
const issuePreview = 3

// prNumber is the request's number as Renovate logged it: a plain number, or
// an object with one.
func (event renovateEvent) prNumber() string {
	var n json.Number
	if err := json.Unmarshal(event.PR, &n); err == nil {
		return n.String()
	}

	var obj struct {
		Number json.Number `json:"number"`
	}

	if err := json.Unmarshal(event.PR, &obj); err == nil && obj.Number != "" {
		return obj.Number.String()
	}

	return ""
}

// repoCounts is what one repository's run amounted to.
type repoCounts struct {
	created, updated, warnings, errors int
}

// renovateReport turns the log stream into the run's output and tallies it.
type renovateReport struct {
	platform string
	names    map[string]string // repository slug -> project name
	current  string            // the slug whose header is on screen
	counts   map[string]*repoCounts
	order    []string
}

func newRenovateReport(platform string, names map[string]string) *renovateReport {
	return &renovateReport{platform: platform, names: names, counts: map[string]*repoCounts{}}
}

// line handles one raw line of Renovate's stdout.
func (rp *renovateReport) line(raw string) {
	event, ok := parseRenovateLine(raw)
	if !ok {
		if strings.TrimSpace(raw) != "" {
			fmt.Println(run.Dim("    " + raw))
		}

		return
	}

	if event.Repository != "" && event.Repository != rp.current {
		rp.current = event.Repository

		if _, seen := rp.counts[event.Repository]; !seen {
			rp.order = append(rp.order, event.Repository)
			rp.counts[event.Repository] = &repoCounts{}
		}

		fmt.Printf("%s %s\n", run.Arrow(), run.Bold(rp.projectName(event.Repository)))
	}

	rp.tally(event)

	if text := rp.render(event); text != "" {
		fmt.Println(text)
	}
}

func (rp *renovateReport) projectName(slug string) string {
	if name, ok := rp.names[slug]; ok {
		return name
	}

	return slug
}

func (rp *renovateReport) tally(event renovateEvent) {
	c := rp.counts[event.Repository]
	if c == nil {
		c = &repoCounts{}
		rp.counts[event.Repository] = c
	}

	switch {
	case event.Msg == "PR created":
		c.created++
	case event.Msg == "PR updated":
		c.updated++
	case event.Level >= logError || event.Msg == msgArtifactErrors:
		c.errors++
	case event.Level == logWarn:
		c.warnings++
	}
}

// msgArtifactErrors is Renovate's word for a lock file it could not update
// — uv lock, go mod tidy, whichever manager — logged as a warning when the
// merge request opens with the package file changed and the lock file not.
// A merge request whose pipeline cannot pass is an error here.
const msgArtifactErrors = "artifactErrors"

// render is one event in rt's words, empty for the lines that say nothing
// worth a line of their own.
func (rp *renovateReport) render(event renovateEvent) string {
	switch {
	case event.Msg == msgArtifactErrors:
		return renderIssues("    "+run.Fail()+" lock file not updated", event)
	case event.Level >= logError:
		return renderIssues("    "+run.Fail()+" "+withReason(event), event)
	case event.Level == logWarn:
		return rp.renderWarning(event)
	case event.Msg == "PR created" || event.Msg == "PR updated":
		return fmt.Sprintf("    %s %s: %s",
			prLabel(rp.platform, event.prNumber()), strings.TrimPrefix(event.Msg, "PR "), run.Green(event.PRTitle))
	case event.Msg == "Branch created" || event.Msg == "Branch updated":
		return fmt.Sprintf("    branch %s %s", planRef(event.Branch), strings.TrimPrefix(event.Msg, "Branch "))
	case event.Msg == "Dependency extraction complete":
		return "    " + extractionSummary(event)
	case strings.HasPrefix(event.Msg, "DRY-RUN:"):
		return "    " + event.Msg
	case event.Msg == "Repository started" || event.Msg == "Repository finished":
		return ""
	case event.Level <= logInfo:
		return run.Dim("    " + event.Msg)
	default:
		return "    " + event.Msg
	}
}

func (rp *renovateReport) renderWarning(event renovateEvent) string {
	head := "    " + run.Warn() + " " + withReason(event)
	if len(event.Files) > 0 {
		head += " (" + strings.Join(event.Files, ", ") + ")"
	}

	return renderIssues(head, event)
}

// renderIssues is a head line with the event's issues under it, the first
// few: validation errors and warnings, and the lock files that failed with
// the line of the tool's stderr that says why.
func renderIssues(head string, event renovateEvent) string {
	issues := append(issueMessages(event.Errors), issueMessages(event.Warnings)...)
	for _, artifact := range event.ArtifactErrors {
		issues = append(issues, artifact.LockFile+": "+errorLine(artifact.Stderr))
	}

	lines := make([]string, 0, issuePreview+2) //nolint:mnd // the head and the "more" line
	lines = append(lines, head)

	for i, issue := range issues {
		if i == issuePreview && len(issues) > issuePreview+1 {
			lines = append(lines, fmt.Sprintf("      ... and %d more", len(issues)-issuePreview))

			break
		}

		lines = append(lines, "      - "+issue)
	}

	return strings.Join(lines, "\n")
}

// errorLine is the line of a tool's stderr that says what went wrong: the
// "error" line when there is one — uv and go put the cause there — else
// the first line with anything on it.
func errorLine(text string) string {
	lines := strings.Split(text, "\n")
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "error") {
			return strings.TrimSpace(line)
		}
	}

	for _, line := range lines {
		if strings.TrimSpace(line) != "" {
			return strings.TrimSpace(line)
		}
	}

	return "no output"
}

// withReason is the message with the error Renovate attached to it, if any.
func withReason(event renovateEvent) string {
	reason := firstNonEmptyString(event.ErrorMessage, event.Err.Message)
	if reason == "" || reason == event.Msg {
		return event.Msg
	}

	return event.Msg + ": " + reason
}

// extractionSummary says what Renovate found to look after, per manager.
func extractionSummary(event renovateEvent) string {
	managers := make([]string, 0, len(event.Stats.Managers))
	for name, m := range event.Stats.Managers {
		managers = append(managers, fmt.Sprintf("%s %d", name, m.DepCount))
	}

	sort.Strings(managers)

	text := fmt.Sprintf("%d %s in %d %s",
		event.Stats.Total.DepCount, plural(event.Stats.Total.DepCount, "dependency", "dependencies"),
		event.Stats.Total.FileCount, plural(event.Stats.Total.FileCount, "file", "files"))
	if len(managers) > 0 {
		text += " (" + strings.Join(managers, ", ") + ")"
	}

	if event.BaseBranch != "" {
		text += " on " + planRef(event.BaseBranch)
	}

	return text
}

// prLabel is how the platform calls a request: GitLab !7, GitHub #7.
func prLabel(platform, number string) string {
	if platform == "github" {
		return "PR #" + number
	}

	return "MR !" + number
}

// finish prints the tally and says whether the run counts as failed: a
// repository Renovate logged an error for, or an exit status with no error
// line to explain it.
func (rp *renovateReport) finish(exitErr error) error {
	var created, updated, warnings, errors int

	var failed []string

	// The lines with no repository — an initialization error, a bad global
	// config — count too; they fail the run as a whole.
	for _, slug := range append([]string{""}, rp.order...) {
		c := rp.counts[slug]
		if c == nil {
			continue
		}

		created, updated, warnings, errors = created+c.created, updated+c.updated, warnings+c.warnings, errors+c.errors

		if c.errors > 0 {
			failed = append(failed, firstNonEmptyString(rp.projectName(slug), "the run itself"))
		}
	}

	fmt.Printf("    %d %s created, %d updated, %d warning(s), %d error(s)\n",
		created, plural(created, "MR", "MRs"), updated, warnings, errors)

	switch {
	case len(failed) > 0:
		return fmt.Errorf("%w %s", errRenovateFailed, strings.Join(failed, ", "))
	case exitErr != nil:
		return fmt.Errorf("%w %w", errRenovateExit, exitErr)
	default:
		return nil
	}
}
