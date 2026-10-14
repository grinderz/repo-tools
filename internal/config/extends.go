package config

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// A release config repeats the dev config but for the release branches and
// a few messages, and the next release repeats the last one. extends names
// the file a config builds on, so the shared blocks live once: the overlay's
// keys go over the base's, maps key by key, lists as a whole, and the
// projects by name, field by field — a project the base lacks is added at
// the end. The merge works on the YAML node trees, so a scalar keeps the
// text it was written with (3.10 stays 3.10) and the merged document is
// then read like any other, unknown keys refused.

// Document is the merged YAML of a config file, extends resolved: what Load
// reads, and what rt config prints.
func Document(path string) ([]byte, error) {
	root, err := readDocument(path, nil)
	if err != nil {
		return nil, err
	}

	out, err := yaml.Marshal(root)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	return out, nil
}

// readDocument reads one file's mapping and merges it over its base, if it
// names one; chain is the files already on the way here, for a cycle.
func readDocument(path string, chain []string) (*yaml.Node, error) {
	if slices.Contains(chain, path) {
		return nil, fmt.Errorf("%w: %s -> %s", errExtendsCycle, strings.Join(chain, " -> "), path)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w%s", path, err, quotingHint(err))
	}

	mapping, err := documentMapping(&doc)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	root := resolveAliases(mapping, nil)

	// Merged into a base that has the key, a key written twice would fold
	// into one and go unseen, where a plain file is refused; two overlay
	// projects of one name the same way. Before extends is taken out, so
	// a second extends is a duplicate too.
	if key := duplicateKey(root); key != "" {
		return nil, fmt.Errorf("%s: mapping key %q %w", path, key, errKeyTwice)
	}

	if name := duplicateProjectName(root); name != "" {
		return nil, fmt.Errorf("%s: projects: %w %q", path, errDuplicateName, name)
	}

	base := takeKey(root, "extends")
	if base == nil {
		return root, nil
	}

	if base.Kind != yaml.ScalarNode || base.Value == "" {
		return nil, fmt.Errorf("%s: extends: %w", path, errExtendsNotAPath)
	}

	basePath := expandHome(base.Value)
	if !filepath.IsAbs(basePath) {
		basePath = filepath.Join(filepath.Dir(path), basePath)
	}

	baseRoot, err := readDocument(basePath, append(chain, path))
	if err != nil {
		return nil, err
	}

	mergeMappings(baseRoot, root)

	return baseRoot, nil
}

// documentMapping is the top-level mapping of a parsed document, an empty
// one for an empty file; a document that is a list or a scalar is not a
// config.
func documentMapping(doc *yaml.Node) (*yaml.Node, error) {
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}, nil
	}

	if top := doc.Content[0]; top.Kind == yaml.MappingNode {
		return top, nil
	}

	return nil, errNotAMapping
}

// duplicateKey is the first key a mapping of the tree writes twice, ""
// when none.
func duplicateKey(node *yaml.Node) string {
	switch node.Kind {
	case yaml.MappingNode:
		seen := map[string]bool{}

		for pos := 0; pos+1 < len(node.Content); pos += pairWidth {
			key := node.Content[pos].Value
			if seen[key] {
				return key
			}

			seen[key] = true

			if dup := duplicateKey(node.Content[pos+1]); dup != "" {
				return dup
			}
		}
	case yaml.SequenceNode:
		for _, item := range node.Content {
			if dup := duplicateKey(item); dup != "" {
				return dup
			}
		}
	case yaml.DocumentNode, yaml.ScalarNode, yaml.AliasNode:
	}

	return ""
}

// duplicateProjectName is the first project name a mapping's projects list
// twice, "" when none.
func duplicateProjectName(mapping *yaml.Node) string {
	pos := indexOfKey(mapping, "projects")
	if pos < 0 || mapping.Content[pos+1].Kind != yaml.SequenceNode {
		return ""
	}

	seen := map[string]bool{}

	for _, project := range mapping.Content[pos+1].Content {
		name := scalarOf(project, "name")
		if name != "" && seen[name] {
			return name
		}

		seen[name] = true
	}

	return ""
}

// resolveAliases replaces every alias with a copy of what it names, so two
// files' anchors cannot collide once their trees are one; anchors are
// dropped with them.
func resolveAliases(node *yaml.Node, seen []*yaml.Node) *yaml.Node {
	if node == nil {
		return nil
	}

	if node.Kind == yaml.AliasNode {
		if slices.Contains(seen, node.Alias) {
			return &yaml.Node{
				Kind:  yaml.ScalarNode,
				Tag:   "!!null",
				Value: "null",
			} // a cycle: yaml.v3 refuses these on decode anyway
		}

		return resolveAliases(node.Alias, append(seen, node.Alias))
	}

	out := *node
	out.Anchor = ""
	out.Content = make([]*yaml.Node, 0, len(node.Content))

	for _, child := range node.Content {
		out.Content = append(out.Content, resolveAliases(child, seen))
	}

	return &out
}

// takeKey removes a key from a mapping and returns its value, nil when the
// key is not there.
func takeKey(mapping *yaml.Node, key string) *yaml.Node {
	pos := indexOfKey(mapping, key)
	if pos < 0 {
		return nil
	}

	value := mapping.Content[pos+1]
	mapping.Content = slices.Delete(mapping.Content, pos, pos+pairWidth)

	return value
}

// pairWidth is a mapping entry in a node's content: the key and its value.
const pairWidth = 2

// indexOfKey is the position of a key in a mapping's content (its value is
// the next entry), -1 when absent.
func indexOfKey(mapping *yaml.Node, key string) int {
	for pos := 0; pos+1 < len(mapping.Content); pos += pairWidth {
		if mapping.Content[pos].Value == key {
			return pos
		}
	}

	return -1
}

// mergeMappings puts over on top of base, in place: maps merge key by key,
// projects by name, anything else is replaced; new keys go at the end.
func mergeMappings(base, over *yaml.Node) {
	for pos := 0; pos+1 < len(over.Content); pos += pairWidth {
		key, value := over.Content[pos], over.Content[pos+1]

		found := indexOfKey(base, key.Value)
		if found < 0 {
			base.Content = append(base.Content, key, value)

			continue
		}

		current := base.Content[found+1]

		switch {
		case key.Value == "projects" && current.Kind == yaml.SequenceNode && value.Kind == yaml.SequenceNode:
			mergeProjects(current, value)
		case current.Kind == yaml.MappingNode && value.Kind == yaml.MappingNode:
			mergeMappings(current, value)
		default:
			base.Content[found+1] = value
		}
	}
}

// mergeProjects keeps the base's order and merges each overlay project
// into the one of the same name, or appends it.
func mergeProjects(base, over *yaml.Node) {
	for _, project := range over.Content {
		name := scalarOf(project, "name")

		found := false

		for _, existing := range base.Content {
			if name != "" && existing.Kind == yaml.MappingNode && scalarOf(existing, "name") == name {
				mergeMappings(existing, project)

				found = true

				break
			}
		}

		if !found {
			base.Content = append(base.Content, project)
		}
	}
}

// scalarOf is the scalar value under key in a mapping, empty otherwise.
func scalarOf(mapping *yaml.Node, key string) string {
	if mapping.Kind != yaml.MappingNode {
		return ""
	}

	if pos := indexOfKey(mapping, key); pos >= 0 && mapping.Content[pos+1].Kind == yaml.ScalarNode {
		return mapping.Content[pos+1].Value
	}

	return ""
}
