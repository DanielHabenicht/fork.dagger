package fsutil

import (
	"context"
	"io"
	gofs "io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// countingFS wraps an FS and records every path the underlying Walk yields, so
// tests can assert whether an excluded directory was descended into (pruned).
type countingFS struct {
	inner   FS
	visited []string
}

func (c *countingFS) Walk(ctx context.Context, target string, fn gofs.WalkDirFunc) error {
	return c.inner.Walk(ctx, target, func(p string, d gofs.DirEntry, err error) error {
		c.visited = append(c.visited, filepath.ToSlash(p))
		return fn(p, d, err)
	})
}

func (c *countingFS) Open(p string) (io.ReadCloser, error) { return c.inner.Open(p) }

func writeTree(t *testing.T, root string, files ...string) {
	t.Helper()
	for _, f := range files {
		p := filepath.Join(root, filepath.FromSlash(f))
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte("x"), 0o644))
	}
}

// runFilter writes tree under a temp dir, walks it through a filterFS with the
// given include/exclude patterns, and returns the kept files (slash-separated,
// relative) plus every path the underlying walk visited.
func runFilter(t *testing.T, tree, include, exclude []string) (kept, visited []string) {
	t.Helper()
	root := t.TempDir()
	writeTree(t, root, tree...)

	base, err := NewFS(root)
	require.NoError(t, err)
	counting := &countingFS{inner: base}
	ffs, err := NewFilterFS(counting, &FilterOpt{IncludePatterns: include, ExcludePatterns: exclude})
	require.NoError(t, err)

	err = ffs.Walk(context.Background(), "/", func(p string, d gofs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d != nil && !d.IsDir() {
			kept = append(kept, filepath.ToSlash(p))
		}
		return nil
	})
	require.NoError(t, err)
	return kept, counting.visited
}

func descendedInto(visited []string, dir string) bool {
	prefix := dir + "/"
	for _, v := range visited {
		if strings.HasPrefix(v, prefix) {
			return true
		}
	}
	return false
}

// TestFilterFS is a declarative table of input tree + include/exclude patterns
// -> expected surviving files. wantPruned lists directories the walk must skip
// entirely (the pruning optimization); wantWalked lists directories the walk
// must still descend into (a re-include reaches inside them).
func TestFilterFS(t *testing.T) {
	cases := []struct {
		name       string
		tree       []string
		include    []string
		exclude    []string
		wantKept   []string
		wantPruned []string
		wantWalked []string
	}{
		{
			name:       "issue #13753: keep only webp, drop node_modules",
			tree:       []string{"a/img.webp", "a/node_modules/dep.js", "a/node_modules/icon.webp", "b/img.webp"},
			exclude:    []string{"**", "!**/*.webp", "**/node_modules"},
			wantKept:   []string{"a/img.webp", "b/img.webp"},
			wantPruned: []string{"a/node_modules"},
		},
		{
			name:       "exclude node_modules directory",
			tree:       []string{"src/main.go", "node_modules/left-pad/index.js"},
			exclude:    []string{"**/node_modules"},
			wantKept:   []string{"src/main.go"},
			wantPruned: []string{"node_modules"},
		},
		{
			name:       "exclude node_modules contents (X/** form)",
			tree:       []string{"src/main.go", "node_modules/dep/index.js"},
			exclude:    []string{"**/node_modules/**"},
			wantKept:   []string{"src/main.go"},
			wantPruned: []string{"node_modules"},
		},
		{
			name:       "trailing-slash directory exclude",
			tree:       []string{"src/main.go", "node_modules/x.js"},
			exclude:    []string{"node_modules/"},
			wantKept:   []string{"src/main.go"},
			wantPruned: []string{"node_modules"},
		},
		{
			name:       "include webp + exclude node_modules (option F)",
			tree:       []string{"a/img.webp", "a/readme.md", "a/node_modules/icon.webp"},
			include:    []string{"**/*.webp"},
			exclude:    []string{"**/node_modules"},
			wantKept:   []string{"a/img.webp"},
			wantPruned: []string{"a/node_modules"},
		},
		{
			name:       "re-include after exclude keeps nested match (must descend)",
			tree:       []string{"node_modules/dep/index.js", "node_modules/dep/icon.webp"},
			exclude:    []string{"**/node_modules", "!**/*.webp"},
			wantKept:   []string{"node_modules/dep/icon.webp"},
			wantWalked: []string{"node_modules"},
		},
		{
			name:       "re-include reaching subtree from an ancestor (must descend)",
			tree:       []string{"apps/web/app.tsx", "apps/web/node_modules/react.js", "apps/web/build/asset.js", "apps/web/build/robots.txt", "apps/api/main.py"},
			exclude:    []string{"apps", "!apps/web/**", "apps/web/build/**", "!apps/web/build/robots.txt"},
			wantKept:   []string{"apps/web/app.tsx", "apps/web/node_modules/react.js", "apps/web/build/robots.txt"},
			wantWalked: []string{"apps/web/node_modules"},
		},
		{
			name:     "deep alternation exclude/re-include/exclude/re-include",
			tree:     []string{"a/x.txt", "a/b/y.txt", "a/b/c/z.txt", "a/b/c/keep.txt"},
			exclude:  []string{"a", "!a/b", "a/b/c", "!a/b/c/keep.txt"},
			wantKept: []string{"a/b/y.txt", "a/b/c/keep.txt"},
		},
		{
			name:       "allowlist: exclude all, re-include src + a file, re-drop generated",
			tree:       []string{"src/main.go", "src/generated/api.go", "dist/bundle.js", "package.json"},
			exclude:    []string{"**", "!src/**", "!package.json", "src/generated/**"},
			wantKept:   []string{"src/main.go", "package.json"},
			wantPruned: []string{"dist", "src/generated"},
		},
		{
			name:     "include *.go but not tests",
			tree:     []string{"a.go", "a_test.go", "pkg/b.go", "pkg/b_test.go", "readme.md"},
			include:  []string{"**/*.go", "!**/*_test.go"},
			wantKept: []string{"a.go", "pkg/b.go"},
		},
		{
			name:       "multiple folders excluded, re-include one file in two of them",
			tree:       []string{"node_modules/x.js", "dist/bundle.js", "dist/keep.js", "coverage/lcov.info", "coverage/raw.dat"},
			exclude:    []string{"node_modules", "dist", "coverage", "!dist/keep.js", "!coverage/lcov.info"},
			wantKept:   []string{"dist/keep.js", "coverage/lcov.info"},
			wantPruned: []string{"node_modules"},
			wantWalked: []string{"dist", "coverage"},
		},
		{
			name:       "re-include in a different folder still prunes node_modules",
			tree:       []string{"node_modules/x.js", "logs/keep.txt", "logs/app.log"},
			exclude:    []string{"node_modules", "logs", "!logs/keep.txt"},
			wantKept:   []string{"logs/keep.txt"},
			wantPruned: []string{"node_modules"},
			wantWalked: []string{"logs"},
		},
		{
			name:       "partial-match pattern does not prune the dir",
			tree:       []string{"build/top.o", "build/sub/deep.txt"},
			exclude:    []string{"build/*.o"},
			wantKept:   []string{"build/sub/deep.txt"},
			wantWalked: []string{"build"},
		},
		{
			name:     "include a subtree, exclude nested privates but keep one",
			tree:     []string{"src/index.ts", "src/generated/private/keys.pem", "src/generated/private/schema.json", "other/file.ts"},
			include:  []string{"src/**"},
			exclude:  []string{"src/generated/private/**", "!src/generated/private/schema.json"},
			wantKept: []string{"src/index.ts", "src/generated/private/schema.json"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			kept, visited := runFilter(t, c.tree, c.include, c.exclude)
			require.ElementsMatch(t, c.wantKept, kept, "surviving files")
			for _, d := range c.wantPruned {
				require.False(t, descendedInto(visited, d), "expected %q to be pruned (not descended into)", d)
			}
			for _, d := range c.wantWalked {
				require.True(t, descendedInto(visited, d), "expected %q to be walked (a re-include reaches inside)", d)
			}
		})
	}
}
