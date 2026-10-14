package cli

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/grinderz/repo-tools/internal/config"
	"github.com/grinderz/repo-tools/internal/gitx"
	"github.com/grinderz/repo-tools/internal/run"
)

// A shared Renovate preset — extends: ["local>group/tooling//renovate/default"]
// — is read by Renovate from the preset repository's default branch unless
// the name ends in #ref. On a release branch that is the wrong branch: the
// rules would move with the tooling's dev branch while every other internal
// dependency is frozen. deps freeze pins such a preset of a configured
// project to the branch that project works on in this config, the rule the
// module pins follow, and repo check holds the file to it. A preset of a
// repository outside the config is Renovate's, like any other extends.

// presetString finds the local> presets in a config file's text, quotes
// included, so a ref is rewritten in place and the file keeps its layout.
var presetString = regexp.MustCompile(`"local>[^"]*"`)

// presetChange is one preset ref the freeze moves.
type presetChange struct {
	Name     string // the preset as written, without its ref
	Provider *config.Project
	From     string // the ref written, "" for the default branch
	To       string // the ref this config wants, "" for the default branch
}

func (c presetChange) String() string {
	return fmt.Sprintf("preset %s: %s -> %s", c.Name, presetRefName(c.From), planRef(presetRefName(c.To)))
}

// presetRefName is how a preset ref is named in messages.
func presetRefName(ref string) string {
	if ref == "" {
		return "default branch"
	}

	return ref
}

// presetRef is the ref a preset of a configured project carries in this
// config: none when the project works on its dev branch here — Renovate
// then reads the default branch, which repo check holds to the dev branch
// — and that branch otherwise.
func presetRef(provider *config.Project) string {
	if provider.TargetBranch() == provider.DevBranch {
		return ""
	}

	return provider.TargetBranch()
}

// rewritePresets moves every local> preset of a configured project in text
// onto that project's ref; the text comes back as it was when nothing
// moves. host is the platform the file's project is on, which a local>
// preset is local to.
func rewritePresets(rctx *run.Ctx, host, text string) (string, []presetChange) {
	var changes []presetChange

	out := presetString.ReplaceAllStringFunc(text, func(quoted string) string {
		name := strings.Trim(quoted, `"`)

		preset, isLocal := parseLocalPreset(name)
		if !isLocal {
			return quoted
		}

		provider := projectBySlug(rctx.Cfg.Projects, host, preset.Slug)
		if provider == nil {
			return quoted
		}

		from, want := preset.Ref, presetRef(provider)
		if from == want {
			return quoted
		}

		preset.Ref = ""
		changes = append(changes, presetChange{Name: preset.String(), Provider: provider, From: from, To: want})

		preset.Ref = want

		return `"` + preset.String() + `"`
	})

	return out, changes
}

// presetHost is the platform host of a project's remote, "" when the url
// has none; a local> preset then names nothing rt knows.
func presetHost(p *config.Project) string {
	host, _, _ := config.SplitRemote(p.Git)

	return host
}

// json5Unfrozen is said of a JSON5 config file: its strings may be single
// quoted, so the refs are not rewritten in it.
const json5Unfrozen = "is JSON5, preset refs not frozen"

// planPresets describes what freezing the preset refs on ref would do; it
// reads the branch being frozen, as the module pins plan does. No config
// file is nothing to pin: deps renovate reports that itself. The file is
// pinned whether or not deps renovate runs on the project: whoever runs
// Renovate on the release branch reads it.
func planPresets(rctx *run.Ctx, r gitx.Repo, p *config.Project, ref string) ([]string, error) {
	file := renovateConfigFile(r, ref)
	if file == "" {
		return nil, nil
	}

	if strings.HasSuffix(file, ".json5") {
		return []string{file + " " + json5Unfrozen}, nil
	}

	text, _, err := r.FileAt(ref, file)
	if err != nil {
		return nil, err
	}

	_, changes := rewritePresets(rctx, presetHost(p), text)

	lines := make([]string, 0, len(changes))
	for _, c := range changes {
		lines = append(lines, fmt.Sprintf("%s: %s", file, c))
	}

	return lines, nil
}

// freezePresets rewrites the preset refs in the checkout's config file.
func freezePresets(rctx *run.Ctx, r gitx.Repo, p *config.Project) error {
	file := renovateConfigFile(r, "")
	if file == "" {
		return nil
	}

	if strings.HasSuffix(file, ".json5") {
		fmt.Printf("    %s %s %s\n", run.Warn(), file, json5Unfrozen)

		return nil
	}

	text, _, err := r.FileAt("", file)
	if err != nil {
		return err
	}

	updated, changes := rewritePresets(rctx, presetHost(p), text)
	if len(changes) == 0 {
		return nil
	}

	if err := rewriteFile(r, file, updated); err != nil {
		return err
	}

	for _, c := range changes {
		fmt.Printf("    %s: %s\n", file, c)
	}

	return nil
}

// checkPresetRefs reports a preset of a configured project that reads
// another branch of it than this config has it on: on a release branch
// one deps freeze has not pinned, on the dev branch one left pinned.
func checkPresetRefs(rctx *run.Ctx, p *config.Project, file, branch, text string) []string {
	_, changes := rewritePresets(rctx, presetHost(p), text)

	issues := make([]string, 0, len(changes))

	for _, c := range changes {
		var what string

		if c.To == "" {
			what = fmt.Sprintf("pins %s to %s, which follows its default branch (%s) here: drop the ref",
				c.Provider.Name, c.From, c.Provider.DevBranch)
		} else {
			what = fmt.Sprintf("reads %s's %s, this config has %s on %s: deps freeze pins it",
				c.Provider.Name, presetRefName(c.From), c.Provider.Name, c.To)
		}

		issues = append(issues, fmt.Sprintf("renovate: %s on %s: preset %s %s", file, branch, c.Name, what))
	}

	return issues
}
