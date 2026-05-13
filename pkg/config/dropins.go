package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

type dropInFile struct {
	Path string
	Node *yaml.Node
}

// discoverDropIns returns individual drop-in file nodes in lexicographic order.
func discoverDropIns(dir string) ([]dropInFile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading drop-in directory %s: %w", dir, err)
	}

	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".yml") {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	var files []dropInFile
	for _, name := range names {
		path := filepath.Join(dir, name)
		node, err := loadYAMLFile(path)
		if err != nil {
			return nil, fmt.Errorf("loading drop-in %s: %w", name, err)
		}
		if node != nil {
			files = append(files, dropInFile{Path: path, Node: node})
		}
	}
	return files, nil
}

// loadDropIns reads all YAML files from dir in lexicographic order and
// deep-merges them into a single node.
func loadDropIns(dir string) (*yaml.Node, error) {
	files, err := discoverDropIns(dir)
	if err != nil {
		return nil, err
	}
	var result *yaml.Node
	for _, f := range files {
		result, err = deepMerge(result, f.Node)
		if err != nil {
			return nil, fmt.Errorf("merging drop-in %s: %w", filepath.Base(f.Path), err)
		}
	}
	return result, nil
}

func loadYAMLFile(path string) (*yaml.Node, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	if len(data) == 0 {
		return nil, nil
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if len(doc.Content) == 0 {
		return nil, nil
	}
	return doc.Content[0], nil
}
