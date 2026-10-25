// Package live keeps a code graph in memory and rebuilds it whenever a .go
// file changes, so every reader sees the code as it is on disk now.
package live

import (
	"fmt"
	"hash/fnv"
	"io/fs"
	"path/filepath"
	"strings"
	"sync"

	"github.com/datfcknadam/codemap/internal/graph"
)

type Graph struct {
	dir string

	mu sync.Mutex
	g  *graph.Graph
	fp uint64 // fingerprint of the .go files g was built from
}

func New(dir string) *Graph {
	return &Graph{dir: dir}
}

// Get returns the cached graph, rebuilding it first if any .go file or
// go.mod changed since it was built.
func (l *Graph) Get() (*graph.Graph, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fp, err := fingerprint(l.dir)
	if err != nil {
		return nil, err
	}
	if l.g != nil && fp == l.fp {
		return l.g, nil
	}
	g, err := graph.Load(l.dir)
	if err != nil {
		return nil, err
	}
	l.g, l.fp = g, fp
	return g, nil
}

// fingerprint hashes path, size and mtime of every Go source and module file.
// It catches edits, new and deleted files in a few tens of milliseconds.
func fingerprint(dir string) (uint64, error) {
	h := fnv.New64a()
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if path != dir && (strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") ||
				name == "node_modules" || name == "vendor" || name == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") && name != "go.mod" && name != "go.sum" {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil // vanished mid-walk; the next call sees the new state
		}
		fmt.Fprintf(h, "%s\x00%d\x00%d\n", path, info.Size(), info.ModTime().UnixNano())
		return nil
	})
	return h.Sum64(), err
}
