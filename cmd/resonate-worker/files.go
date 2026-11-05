package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Limits on the data files (feeders, body files) uploaded alongside a scenario.
const (
	maxDataFiles     = 10
	maxDataFileBytes = 10 << 20 // per file
	maxDataTotal     = 25 << 20 // all files of one task
)

var errDataTooLarge = errors.New("data files are larger than the size limit")

// reservedNames are files the worker or resonate own inside a task workspace; an
// upload may not overwrite them.
var reservedNames = map[string]bool{
	"scenario.yaml": true, taskFile: true, taskFile + ".tmp": true,
	"report.html": true, "report.json": true, "results.jsonl": true, "log.txt": true,
}

// dataFileName validates the name of an uploaded data file: a plain file name,
// no directories, so it lands directly in the task workspace and the scenario
// refers to it by that same name.
func dataFileName(name string) (string, error) {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) || strings.ContainsRune(name, 0) {
		return "", fmt.Errorf("invalid data file name %q: want a plain file name, no directories", name)
	}
	if reservedNames[name] {
		return "", fmt.Errorf("data file name %q is reserved", name)
	}
	return name, nil
}

// saveDataFile writes src into dir as name, refusing to exceed the per-file cap
// or the remaining total budget. It returns the number of bytes written.
func saveDataFile(dir, name string, src io.Reader, budget int64) (int64, error) {
	limit := int64(maxDataFileBytes)
	if budget < limit {
		limit = budget
	}
	dst, err := os.OpenFile(filepath.Join(dir, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if os.IsExist(err) {
			return 0, fmt.Errorf("duplicate data file name %q", name)
		}
		return 0, err
	}
	n, err := io.Copy(dst, io.LimitReader(src, limit+1))
	if cerr := dst.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > limit {
		err = errDataTooLarge
	}
	return n, err
}

// pathKeys are the scenario keys whose value is a single file to read, and
// pathListKeys those whose value is a list of files or directories. feeder.file
// is the one key whose name alone is ambiguous, so it is matched by its parent.
var (
	pathKeys     = map[string]bool{"body_file": true, "raw_body_file": true, "message_file": true, "ca_file": true, "cert_file": true, "key_file": true}
	pathListKeys = map[string]bool{"proto_files": true, "import_paths": true}
)

// checkScenarioPaths rejects a scenario that reads files outside its own task
// workspace. resonate resolves these keys relative to its working directory, so
// relative names without ".." stay inside the workspace, where the uploaded data
// files are. An absolute path would let a submitted scenario read any file the
// worker can (and message_file, body_file and friends then send it to the target).
func checkScenarioPaths(scenario []byte) error {
	var root yaml.Node
	if err := yaml.Unmarshal(scenario, &root); err != nil {
		return nil // not valid YAML: let resonate report it
	}
	return walkPaths(&root, "")
}

func walkPaths(n *yaml.Node, parent string) error {
	switch n.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, c := range n.Content {
			if err := walkPaths(c, parent); err != nil {
				return err
			}
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			key, val := n.Content[i].Value, n.Content[i+1]
			switch {
			case pathKeys[key] || (key == "file" && parent == "feeder"):
				if val.Kind == yaml.ScalarNode {
					if err := checkRelPath(key, val.Value); err != nil {
						return err
					}
				}
			case pathListKeys[key]:
				entries := val.Content
				if val.Kind == yaml.ScalarNode {
					entries = []*yaml.Node{val}
				}
				for _, e := range entries {
					if e.Kind == yaml.ScalarNode {
						if err := checkRelPath(key, e.Value); err != nil {
							return err
						}
					}
				}
			}
			if err := walkPaths(val, key); err != nil {
				return err
			}
		}
	}
	return nil
}

func checkRelPath(key, p string) error {
	if filepath.IsAbs(p) || strings.HasPrefix(p, "/") || strings.HasPrefix(p, `\`) || hasDotDot(p) {
		return fmt.Errorf("scenario %s %q must be a plain file name uploaded with the scenario (no absolute paths or \"..\")", key, p)
	}
	return nil
}

func hasDotDot(p string) bool {
	for _, part := range strings.FieldsFunc(p, func(r rune) bool { return r == '/' || r == '\\' }) {
		if part == ".." {
			return true
		}
	}
	return false
}
