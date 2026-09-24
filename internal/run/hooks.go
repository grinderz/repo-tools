package run

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

// A run is mostly waiting: for a question to be answered, for a pipeline,
// for a merge request to be merged. The hooks of the config are shell
// commands rt runs at those moments and when it is done, so a notification
// can call the operator back to the terminal. A hook is a courtesy, never a
// gate: its failure is a warning, and nothing waits for its output.

// The events a hook can be attached to.
const (
	EventAsk  = "ask"  // rt is about to wait for an answer
	EventWait = "wait" // rt is about to wait for something outside: a pipeline, a merge
	EventDone = "done" // the invoked command — a flow counts as one — has finished
)

// The variables a hook command sees, on top of rt's own environment.
const (
	EnvEvent      = "RT_EVENT"
	EnvCommand    = "RT_COMMAND"     // the invoked command with its arguments
	EnvProject    = "RT_PROJECT"     // the project being worked on, when there is one
	EnvProjectDir = "RT_PROJECT_DIR" // its working copy
	EnvMessage    = "RT_MESSAGE"     // the question, what is waited for, or the outcome
	EnvStatus     = "RT_STATUS"      // done: ok, failed or declined
	EnvURL        = "RT_URL"         // wait: the pipeline or the merge request
)

// The done statuses.
const (
	StatusOK       = "ok"
	StatusFailed   = "failed"
	StatusDeclined = "declined"
)

// hookTimeout bounds one hook: a notifier that hangs must not hold a release.
const hookTimeout = 30 * time.Second

// hooks is the configured command list per event, plus what every hook is
// told about the run. Process-wide like the colour switch; the mutex is for
// the tests, which run in parallel.
//
//nolint:gochecknoglobals // one config per process
var hooks = struct {
	sync.RWMutex

	cmds    map[string][]string
	command string // the invoked command, for RT_COMMAND
	project string // the project a step is working on, for RT_PROJECT
	dir     string
}{}

// SetHooks installs the config's hooks and names the invoked command. A dry
// run installs none: a notification about a plan nobody executes is noise.
func SetHooks(cmds map[string][]string, command string, dryRun bool) {
	hooks.Lock()
	defer hooks.Unlock()

	hooks.cmds, hooks.command = nil, command

	if !dryRun {
		hooks.cmds = cmds
	}
}

// setProject names the project the current step works on, for the hooks
// fired from inside it; empty between steps.
func setProject(name, dir string) {
	hooks.Lock()
	defer hooks.Unlock()

	hooks.project, hooks.dir = name, dir
}

// Fire runs the hooks of one event with the given variables, on top of what
// the run knows. Failures are warnings on stderr: a broken notifier is not
// the release's problem.
func Fire(event string, vars map[string]string) {
	hooks.RLock()
	cmds := hooks.cmds[event]
	env := []string{
		EnvEvent + "=" + event,
		EnvCommand + "=" + hooks.command,
		EnvProject + "=" + hooks.project,
		EnvProjectDir + "=" + hooks.dir,
	}
	hooks.RUnlock()

	if len(cmds) == 0 {
		return
	}

	for key, value := range vars {
		env = append(env, key+"="+value)
	}

	for _, command := range cmds {
		if err := runHook(command, env); err != nil {
			fmt.Fprintf(os.Stderr, "%s hook %s: %s: %v\n", Warn(), event, command, err)
		}
	}
}

func runHook(command string, env []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), hookTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", command)

	cmd.Env = append(os.Environ(), env...)

	out, err := cmd.CombinedOutput()
	if err != nil {
		if line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n"); line != "" {
			return fmt.Errorf("%w: %s", err, line)
		}

		return fmt.Errorf("hook: %w", err)
	}

	return nil
}

// ansiRe matches the escape sequences the coloured output carries.
var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// plainText is text without colour codes and without the trailing prompt
// punctuation, the way a notification should read it.
func plainText(s string) string {
	return strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(ansiRe.ReplaceAllString(s, "")), ":"))
}
