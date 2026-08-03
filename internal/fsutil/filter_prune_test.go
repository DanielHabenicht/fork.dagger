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

// walkCase walks root through a filterFS with the given include/exclude
// patterns and returns kept files, kept dirs, and every underlying-walk path.
func walkCase(t *testing.T, root string, include, exclude []string) (keptFiles, keptDirs, visited []string) {
	t.Helper()
	base, err := NewFS(root)
	require.NoError(t, err)
	counting := &countingFS{inner: base}
	ffs, err := NewFilterFS(counting, &FilterOpt{IncludePatterns: include, ExcludePatterns: exclude})
	require.NoError(t, err)

	err = ffs.Walk(context.Background(), "/", func(p string, d gofs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d == nil {
			return nil
		}
		if d.IsDir() {
			keptDirs = append(keptDirs, filepath.ToSlash(p))
		} else {
			keptFiles = append(keptFiles, filepath.ToSlash(p))
		}
		return nil
	})
	require.NoError(t, err)
	return keptFiles, keptDirs, counting.visited
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

// TestFilterFSComplexPatternCombinations asserts correctness (exactly the right
// files kept) across a range of tricky include/exclude/re-include combinations,
// and — where a whole subtree is dropped — that the walk pruned it.
func TestFilterFSComplexPatternCombinations(t *testing.T) {
	tree := []string{
		"apps/app1/image.webp",
		"apps/app1/logo.png",
		"apps/app1/main.ts",
		"apps/app1/node_modules/pkg/index.js",
		"apps/app1/node_modules/pkg/icon.webp",
		"apps/app2/image.webp",
		"apps/app2/node_modules/dep/util.js",
		"apps/app2/vendor/lib/x.js",
		"apps/app2/vendor/lib/keep.txt",
		"README.md",
	}

	type tc struct {
		name    string
		include []string
		exclude []string
		// keptFiles: exact set of files that must survive.
		keptFiles []string
		// prunedDirs: directories the walk must NOT descend into.
		prunedDirs []string
		// walkedDirs: directories the walk MUST descend into (re-include reach).
		walkedDirs []string
	}

	cases := []tc{
		{
			name:    "exclude all but webp, prune node_modules (issue #13753)",
			exclude: []string{"**", "!**/*.webp", "**/node_modules"},
			keptFiles: []string{
				"apps/app1/image.webp",
				"apps/app2/image.webp",
			},
			prunedDirs: []string{"apps/app1/node_modules", "apps/app2/node_modules"},
		},
		{
			name:    "include webp + exclude node_modules dir (option F usage)",
			include: []string{"**/*.webp"},
			exclude: []string{"**/node_modules"},
			keptFiles: []string{
				"apps/app1/image.webp",
				"apps/app2/image.webp",
			},
			prunedDirs: []string{"apps/app1/node_modules", "apps/app2/node_modules"},
		},
		{
			name:    "reverse order: re-include webp AFTER node_modules exclude keeps nested webp",
			exclude: []string{"**", "!**/*.webp"}, // no node_modules re-exclude
			keptFiles: []string{
				"apps/app1/image.webp",
				"apps/app1/node_modules/pkg/icon.webp",
				"apps/app2/image.webp",
			},
			walkedDirs: []string{"apps/app1/node_modules"},
		},
		{
			name:    "node_modules excluded then webp re-included (higher precedence) keeps nested webp",
			exclude: []string{"**/node_modules", "!**/*.webp"},
			keptFiles: []string{
				"apps/app1/image.webp",
				"apps/app1/logo.png",
				"apps/app1/main.ts",
				"apps/app1/node_modules/pkg/icon.webp",
				"apps/app2/image.webp",
				"apps/app2/vendor/lib/x.js",
				"apps/app2/vendor/lib/keep.txt",
				"README.md",
			},
			walkedDirs: []string{"apps/app1/node_modules"},
		},
		{
			name:    "multiple re-includes, prune node_modules and vendor",
			exclude: []string{"**", "!**/*.webp", "!**/*.png", "**/node_modules", "**/vendor"},
			keptFiles: []string{
				"apps/app1/image.webp",
				"apps/app1/logo.png",
				"apps/app2/image.webp",
			},
			prunedDirs: []string{"apps/app1/node_modules", "apps/app2/node_modules", "apps/app2/vendor"},
		},
		{
			name:    "trailing-slash dir exclude prunes",
			exclude: []string{"**", "!**/*.webp", "**/node_modules/"},
			keptFiles: []string{
				"apps/app1/image.webp",
				"apps/app2/image.webp",
			},
			prunedDirs: []string{"apps/app1/node_modules"},
		},
		{
			name:    "exclude everything under node_modules (X/** content form)",
			exclude: []string{"**", "!**/*.webp", "**/node_modules/**"},
			keptFiles: []string{
				"apps/app1/image.webp",
				"apps/app2/image.webp",
			},
			prunedDirs: []string{"apps/app1/node_modules", "apps/app2/node_modules"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := t.TempDir()
			writeTree(t, root, tree...)
			keptFiles, _, visited := walkCase(t, root, c.include, c.exclude)

			require.ElementsMatch(t, c.keptFiles, keptFiles, "kept files mismatch")
			for _, d := range c.prunedDirs {
				require.False(t, descendedInto(visited, d),
					"expected %s to be pruned (not descended into)", d)
			}
			for _, d := range c.walkedDirs {
				require.True(t, descendedInto(visited, d),
					"expected %s to be walked (a re-include reaches inside)", d)
			}
		})
	}
}

// TestFilterFSSubtreeExcludePruning exercises the "X/**" content-only exclude
// form, which does not match the directory node itself, focusing on the
// non-excluded-directory prune path and its guards against over-pruning.
func TestFilterFSSubtreeExcludePruning(t *testing.T) {
	t.Run("prunes subtree, keeps siblings", func(t *testing.T) {
		root := t.TempDir()
		writeTree(t, root,
			"node_modules/pkg/index.js",
			"node_modules/deep/more/x.js",
			"src/main.go",
			"src/util/helper.go",
		)
		keptFiles, _, visited := walkCase(t, root, nil, []string{"node_modules/**"})

		require.ElementsMatch(t, []string{"src/main.go", "src/util/helper.go"}, keptFiles)
		require.False(t, descendedInto(visited, "node_modules"), "node_modules subtree must be pruned")
		require.True(t, descendedInto(visited, "src"), "unrelated src dir must still be walked")
	})

	t.Run("does not prune when re-include reaches into subtree", func(t *testing.T) {
		root := t.TempDir()
		writeTree(t, root,
			"node_modules/pkg/index.js",
			"node_modules/keep/data.json",
		)
		keptFiles, _, visited := walkCase(t, root, nil,
			[]string{"node_modules/**", "!node_modules/keep/**"})

		require.ElementsMatch(t, []string{"node_modules/keep/data.json"}, keptFiles)
		require.True(t, descendedInto(visited, "node_modules"), "must descend to reach re-included keep/")
	})

	t.Run("partial-match pattern does not prune the dir (deeper files survive)", func(t *testing.T) {
		root := t.TempDir()
		writeTree(t, root,
			"build/top.o",
			"build/sub/deep.txt",
		)
		// build/*.o only excludes .o files, not the whole build subtree, so
		// build must be walked and build/sub/deep.txt kept.
		keptFiles, _, visited := walkCase(t, root, nil, []string{"build/*.o"})

		require.Equal(t, []string{"build/sub/deep.txt"}, keptFiles)
		require.True(t, descendedInto(visited, "build"), "build must be walked; build/*.o is not a subtree exclude")
	})

	t.Run("nested subtree excludes prune independently", func(t *testing.T) {
		root := t.TempDir()
		writeTree(t, root,
			"a/node_modules/x.js",
			"a/keep.txt",
			"b/node_modules/y.js",
			"b/keep.txt",
		)
		keptFiles, _, visited := walkCase(t, root, nil, []string{"**/node_modules/**"})

		require.ElementsMatch(t, []string{"a/keep.txt", "b/keep.txt"}, keptFiles)
		require.False(t, descendedInto(visited, "a/node_modules"))
		require.False(t, descendedInto(visited, "b/node_modules"))
	})
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
