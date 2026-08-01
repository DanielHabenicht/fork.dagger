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
// tests can assert whether an excluded directory was descended into.
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

// walkFiltered walks root through a filterFS with the given exclude patterns
// and returns the kept files and every path the underlying walk visited.
func walkFiltered(t *testing.T, root string, exclude []string) (kept, visited []string) {
	t.Helper()
	base, err := NewFS(root)
	require.NoError(t, err)
	counting := &countingFS{inner: base}
	ffs, err := NewFilterFS(counting, &FilterOpt{ExcludePatterns: exclude})
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

func TestFilterFSPrunesExcludedDir(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root,
		"apps/app1/image.webp",
		"apps/app1/node_modules/junk.js",
		"apps/app1/node_modules/nested.webp",
		"apps/app2/image.webp",
		"apps/app2/node_modules/junk.js",
	)

	// dagger/dagger#13753: a wildcard re-include (!**/*.webp) must not disable
	// pruning of node_modules, which the later **/node_modules overrides.
	kept, visited := walkFiltered(t, root, []string{"**", "!**/*.webp", "**/node_modules"})

	require.ElementsMatch(t, []string{"apps/app1/image.webp", "apps/app2/image.webp"}, kept,
		"the nested webp inside node_modules must not be kept (exclude overrides re-include)")

	for _, v := range visited {
		require.NotContains(t, v, "node_modules/",
			"walker descended into an excluded node_modules: %s", v)
	}
}

func TestFilterFSPrunesExcludedDirExcludeOnly(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root,
		"apps/app1/image.webp",
		"apps/app1/node_modules/junk.js",
	)

	// A plain directory exclude with no re-include should prune the subtree.
	kept, visited := walkFiltered(t, root, []string{"**/node_modules"})

	require.Contains(t, kept, "apps/app1/image.webp")
	for _, v := range visited {
		require.NotContains(t, v, "node_modules/",
			"walker descended into an excluded node_modules: %s", v)
	}
}

func TestFilterFSDoesNotPruneWhenReincludeReachesInside(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root,
		"node_modules/junk.js",
		"node_modules/deep/keep.keep",
	)

	// The re-include has HIGHER precedence than the exclude and targets files
	// inside node_modules, so the dir must be walked, not pruned.
	kept, visited := walkFiltered(t, root, []string{"**/node_modules", "!**/node_modules/**/*.keep"})

	require.Equal(t, []string{"node_modules/deep/keep.keep"}, kept)

	descended := false
	for _, v := range visited {
		if strings.Contains(v, "node_modules/") {
			descended = true
			break
		}
	}
	require.True(t, descended, "walker must descend into node_modules to reach the re-included .keep file")
}

func TestFilterFSDoesNotPruneWhenPrefixReincludeInside(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root,
		"build/out.o",
		"build/keep/file.txt",
	)

	// Literal-prefix re-include pointing inside the excluded dir: must descend.
	kept, _ := walkFiltered(t, root, []string{"build", "!build/keep"})

	require.Equal(t, []string{"build/keep/file.txt"}, kept)
}

func TestFilterFSPrunesWhenReincludeIsElsewhere(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root,
		"node_modules/junk.js",
		"logs/keep.txt",
		"logs/other.log",
	)

	// A re-include that targets a different directory must not prevent pruning
	// node_modules.
	kept, visited := walkFiltered(t, root, []string{"node_modules", "logs", "!logs/keep.txt"})

	require.ElementsMatch(t, []string{"logs/keep.txt"}, kept)
	for _, v := range visited {
		require.NotContains(t, v, "node_modules/",
			"walker descended into an excluded node_modules: %s", v)
	}
}
