package cli

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/grinderz/repo-tools/internal/config"
)

// A shared preset — extends: ["local>group/tooling//renovate/default"] —
// is fetched by Renovate from the platform, so until it is pushed every
// run that extends it fails. Under --local-config rt reads such a preset
// from the project it names, when that project is in the config — its
// working copy, or the branch a #ref names — and puts it into the file the
// way Renovate would have: the preset's keys under the file's own, the
// package rules the preset's first, the preset's own extends in its place.
// A preset of a repository outside the config stays Renovate's to fetch.

// localPresetRe is Renovate's local> preset: the repository, then either
// :name for name.json at the root or //path to the file (default.json at
// the root with neither), an optional #ref — the preset repository's
// default branch without one — and the optional (parameters) of a
// parameterised preset.
var localPresetRe = regexp.MustCompile(
	`^local>([^/:#(]+(?:/[^/:#(]+)+?)(?:(?::([^/#(]+))|(?://([^#(]+)))?(?:#([^(]+))?(\(.*\))?$`)

// localPreset is a local> preset taken apart.
type localPreset struct {
	Slug   string
	Name   string // the :name form
	Path   string // the //path form
	Ref    string
	Params string
}

// parseLocalPreset reads a local> preset; ok is false for any other extends.
func parseLocalPreset(name string) (localPreset, bool) {
	m := localPresetRe.FindStringSubmatch(name)
	if m == nil {
		return localPreset{}, false
	}

	return localPreset{Slug: m[1], Name: m[2], Path: m[3], Ref: m[4], Params: m[5]}, true
}

// String writes the preset back the way it was given, the ref as it is now.
func (p localPreset) String() string {
	out := "local>" + p.Slug

	switch {
	case p.Name != "":
		out += ":" + p.Name
	case p.Path != "":
		out += "//" + p.Path
	}

	if p.Ref != "" {
		out += "#" + p.Ref
	}

	return out + p.Params
}

// file is the file the preset names, or a reason it cannot be read.
func (p localPreset) file() (string, string) {
	if p.Name != "" {
		return presetFile(p.Name)
	}

	return presetFile(p.Path)
}

// presetReader reads a preset file of the project a local> preset names,
// by its remote's group/name: from the working copy, or at ref when the
// preset carries one. where says which, for the plan; ok is false when no
// project in the config is that one; a reason says why the file could not
// be read.
type presetReader func(slug, ref, file string) (data []byte, where string, ok bool, reason string)

// renovateInlinePresets replaces the local> presets of the config's own
// projects in cfg's extends with their content, in place, and names the
// files taken for the plan. chain is the presets on the way here, for a
// cycle. A reason says why a preset could not be taken. The presets go
// into one base first, each over the one before — a later preset wins in
// Renovate, and its rules come after — and the file's own keys over that.
func renovateInlinePresets(cfg map[string]any, read presetReader, chain []string) ([]string, string) {
	entries, _ := cfg["extends"].([]any)

	var (
		extends []any
		taken   []string
	)

	base := map[string]any{}

	for _, entry := range entries {
		name, _ := entry.(string)

		preset, isLocal := parseLocalPreset(name)
		if !isLocal {
			extends = append(extends, entry)

			continue
		}

		slug, ref := preset.Slug, preset.Ref

		file, reason := preset.file()

		data, where, ok, unread := read(slug, ref, file)
		if !ok {
			extends = append(extends, entry) // not ours: Renovate fetches it

			continue
		}

		if reason != "" {
			return nil, name + ": " + reason
		}

		if unread != "" {
			return nil, name + ": " + unread
		}

		key := slug + "/" + file + "#" + ref
		if slices.Contains(chain, key) {
			return nil, name + ": extends itself through " + strings.Join(chain, " -> ")
		}

		body, reason := parsePreset(data, name, file)
		if reason != "" {
			return nil, reason
		}

		nested, reason := renovateInlinePresets(body, read, append(chain, key))
		if reason != "" {
			return nil, reason
		}

		presetExtends, _ := body["extends"].([]any)
		extends = append(extends, presetExtends...)

		mergePreset(body, base)
		base = body

		taken = append(taken, file+" from "+where)
		taken = append(taken, nested...)
	}

	mergePreset(cfg, base)

	if len(extends) > 0 {
		cfg["extends"] = extends
	} else {
		delete(cfg, "extends")
	}

	return taken, ""
}

// presetFile is the file a local> preset's path names: default.json at
// the root, else the path with .json added unless it carries an extension
// already — as Renovate builds it.
func presetFile(path string) (string, string) {
	switch {
	case path == "":
		return "default.json", ""
	case strings.HasSuffix(path, ".json5") || strings.HasSuffix(path, ".jsonc"):
		return "", "a JSON5 preset is Renovate's own to read"
	case strings.HasSuffix(path, ".json"):
		return path, ""
	default:
		return path + ".json", ""
	}
}

// parsePreset reads a preset file as a JSON object, its schema pointer
// and description dropped.
func parsePreset(data []byte, name, file string) (map[string]any, string) {
	var preset map[string]any
	if err := json.Unmarshal(data, &preset); err != nil {
		return nil, fmt.Sprintf("%s: %s is not valid JSON: %v", name, filepath.Base(file), err)
	}

	delete(preset, "$schema")
	delete(preset, "description")

	// The file's own rules are held to this; a preset's would be dropped
	// on the way otherwise.
	if rules, ok := preset["packageRules"]; ok {
		if _, isList := rules.([]any); !isList {
			return nil, fmt.Sprintf("%s: %s: packageRules is not a list", name, filepath.Base(file))
		}
	}

	return preset, ""
}

// presetListKeys are the lists Renovate joins when a config extends a
// preset — the options marked mergeable in its table — the preset's
// entries first; any other list is replaced.
//
//nolint:gochecknoglobals // a fixed table
var presetListKeys = []string{
	"packageRules", "hostRules", "customManagers", "postUpdateOptions", "ignoreDeps", "addLabels",
	"allowedEnv", "additionalReviewers", "commitTrailers", "prBodyNotes", "suppressNotifications", "logLevelRemap",
}

// mergePreset puts a preset under a config the way Renovate resolves one:
// the config's own keys win, the rule lists are joined with the preset's
// first, and a mapping both have is merged key by key, the config's own
// winning. extends is the caller's.
func mergePreset(cfg, preset map[string]any) {
	for key, value := range preset {
		if key == "extends" {
			continue
		}

		own, has := cfg[key]
		if !has {
			cfg[key] = value

			continue
		}

		switch {
		case slices.Contains(presetListKeys, key):
			presetRules, _ := value.([]any)
			ownRules, _ := own.([]any)
			cfg[key] = append(slices.Clone(presetRules), ownRules...)
		default:
			if presetMap, ok := value.(map[string]any); ok {
				if ownMap, ok := own.(map[string]any); ok {
					merged := maps.Clone(presetMap)
					maps.Copy(merged, ownMap)
					cfg[key] = merged
				}
			}
		}
	}
}

// projectPresetReader is presetReader over the config's projects on the
// platform a local> preset is local to: the working copy for a preset
// without a ref, the branch (or tag) for one with it — the local branch
// when there is one, so an unpushed commit on it counts, as it does in the
// working copy. A project of the config that is not cloned is ours all the
// same: the preset is then not read, with the reason.
func projectPresetReader(projects []*config.Project, host string) presetReader {
	return func(slug, ref, file string) ([]byte, string, bool, string) {
		p := projectBySlug(projects, host, slug)
		if p == nil {
			return nil, "", false, ""
		}

		if reason := missingRepo(repoOf(p)); reason != "" {
			return nil, "", true, p.Name + " " + reason
		}

		if ref == "" {
			where := p.Name + "'s working copy"

			data, err := os.ReadFile(filepath.Join(p.Dir(), file))
			if err != nil {
				return nil, where, true, fmt.Sprintf("%s not in %s", filepath.Base(file), where)
			}

			return data, where, true, ""
		}

		where := p.Name + " on " + ref

		r := repoOf(p)

		rev := ref // a tag, or a ref git knows and rt does not
		if !r.LocalBranchExists(ref) && r.RemoteBranchExists(ref) {
			rev = "origin/" + ref
		}

		text, found, err := r.FileAt(rev, file)
		if err != nil || !found {
			return nil, where, true, fmt.Sprintf("%s not in %s", filepath.Base(file), where)
		}

		return []byte(text), where, true, ""
	}
}

// projectBySlug is the project whose remote is group/name on host, nil
// when the config has none: what a local> preset names. An http port is
// not part of the name — an ssh url to the same platform has none.
func projectBySlug(projects []*config.Project, host, slug string) *config.Project {
	for _, p := range projects {
		if h, got, ok := config.SplitRemote(p.Git); ok &&
			strings.EqualFold(hostName(h), hostName(host)) && strings.EqualFold(got, slug) {
			return p
		}
	}

	return nil
}

// hostName is a host without its port.
func hostName(host string) string {
	name, _, _ := strings.Cut(host, ":")

	return name
}
