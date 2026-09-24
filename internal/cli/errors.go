package cli

import "errors"

// The errors below are the fixed part of what a command reports. Each one is
// wrapped with %w at the place it takes in the sentence, so the text reads as
// before while callers — flows, tests — can still ask errors.Is what went
// wrong instead of matching words.
var (
	errUsage          = errors.New("usage")
	errWithIssues     = errors.New("project(s) with issues")
	errStaleDeps      = errors.New("project(s) with stale dependency files")
	errProjectsFailed = errors.New("project(s) failed")

	errNoCommitsGiven = errors.New("no commits given, and choosing them needs a question this run cannot ask")
	errUpToDate       = errors.New("already has everything from")
	errAlreadyPicked  = errors.New("are already in")
	errNotSelected    = errors.New("is selected by")
	errUnknownCommit  = errors.New("unknown commit")
	errAllGivenPicked = errors.New("every commit given is already in")
	errManualResolve  = errors.New("manual conflict resolution required in")

	errUnknownRef     = errors.New("is neither a branch on origin nor a tag")
	errStillRunning   = errors.New("is still running after")
	errPipelineFailed = errors.New("failed")
	errUnknownCI      = errors.New("unknown ci kind")

	errEnvrcBlocked    = errors.New("is blocked, run: direnv allow")
	errCannotFetch     = errors.New("cannot fetch")
	errFetchFailed     = errors.New("fetch failed")
	errNoRemoteBranch  = errors.New("does not exist")
	errPendingRelease  = errors.New("does not exist yet, run release branch to create it")
	errNoReleaseBranch = errors.New("no release branch")
	errBadReleaseName  = errors.New("does not match")
	errNotXY           = errors.New("is not X.Y")
	errNoBranch        = errors.New("exists neither locally nor on origin")
	errUnpushed        = errors.New("local commit(s) that are not on origin")
	errLocalAhead      = errors.New("already exists and has commits that are not on")
	errDiverged        = errors.New("has diverged from origin")
	errDirty           = errors.New("working tree is dirty")
	errDirtyUnattended = errors.New("--allow-dirty needs the diff review and someone to answer it: " +
		"drop --no-diff/--yes, or clean the tree with repo clean")

	errUnknownFlow   = errors.New("is not in the config")
	errFlowInFlow    = errors.New("a flow cannot run another flow")
	errNotACommand   = errors.New("is not a command")
	errGroupStep     = errors.New("is a group, name one of its commands")
	errNotAFlowStep  = errors.New("cannot be a flow step")
	errNotAProject   = errors.New("not a project in this config")
	errUnknownCmd    = errors.New("unknown command")
	errDisabled      = errors.New("is disabled in")
	errMutuallyExcl  = errors.New("are mutually exclusive")
	errBackwards     = errors.New("runs backwards")
	errNotANumber    = errors.New("is not a number")
	errOutOfRange    = errors.New("is out of range")
	errNoSuchSubs    = errors.New("no selected project has any of the submodules")
	errNoSubBranch   = errors.New("has no branch in .gitmodules of")
	errTracksNothing = errors.New("tracks no branch")

	errRebaseStart     = errors.New("rebase failed to start")
	errRebaseContinue  = errors.New("rebase --continue failed with a non-empty index")
	errForeignConflict = errors.New("conflict in non-submodule path")
	errTooDeep         = errors.New("submodules nest deeper than")
	errDeclinedPush    = errors.New("declined pushing")
	errPinUnknown      = errors.New("is unknown to the remote")
	errPinOffBranch    = errors.New("does not contain commit")
	errNoMRTool        = errors.New("cannot open a merge request")
	errNotAStepFlag    = errors.New("is the run's, not a step's")
	errMRClosed        = errors.New("merge request was closed without being merged")
	errNoMRTemplate    = errors.New("merge request template not found")
	errMRNotMerged     = errors.New("merge request is still not merged")
)

// reasonError carries a reason that is already a sentence for the operator,
// the kind missingRepo returns, so it can travel as an error without being
// reworded.
type reasonError string

func (e reasonError) Error() string { return string(e) }

// briefError shows only the first line of err — git's hints and progress stay
// out of a one-line report — while errors.Is and errors.As still see all of it.
type briefError struct{ err error }

func (e briefError) Error() string { return firstLine(e.err.Error()) }

func (e briefError) Unwrap() error { return e.err }
