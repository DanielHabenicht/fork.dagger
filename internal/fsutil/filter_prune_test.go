package fsutil

import (
	"context"
	"io"
	gofs "io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/moby/patternmatcher"
	"github.com/stretchr/testify/require"
)

// oracleKept computes, independently of the walk/prune logic, exactly which of
// the given files survive the include/exclude patterns, using the canonical
// per-file patternmatcher decision (the same rule filterFS.Open applies). The
// pruned walk must keep exactly this set — pruning is only an optimization and
// must never change which files are kept.
func oracleKept(t *testing.T, files, include, exclude []string) []string {
	t.Helper()
	var im, em *patternmatcher.PatternMatcher
	var err error
	if len(include) > 0 {
		im, err = patternmatcher.New(include)
		require.NoError(t, err)
	}
	if len(exclude) > 0 {
		em, err = patternmatcher.New(exclude)
		require.NoError(t, err)
	}
	var kept []string
	for _, f := range files {
		if im != nil {
			m, err := im.MatchesOrParentMatches(f)
			require.NoError(t, err)
			if !m {
				continue
			}
		}
		if em != nil {
			m, err := em.MatchesOrParentMatches(f)
			require.NoError(t, err)
			if m {
				continue
			}
		}
		kept = append(kept, f)
	}
	return kept
}

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

// realWorldTree spans apps/packages/src/node_modules/dist/build/vendor/
// __pycache__/coverage/.terraform/assets so the pattern lists below each have
// both kept and dropped files, including files nested inside excluded dirs and
// inside re-included subtrees.
var realWorldTree = []string{
	"package.json", "package-lock.json", "README.md", "go.mod", "go.sum",
	".terraform.lock.hcl", ".git/config", ".github/workflows/ci.yml",
	"src/index.ts", "src/util.go", "src/util_test.go",
	"src/generated/api.ts", "src/generated/private/schema.json", "src/generated/private/keys.pem",
	"node_modules/left-pad/index.js",
	"dist/.gitkeep", "dist/keep.js", "dist/bundle.js",
	".next/static/chunk.js", "build/manifest.json", "build/main.js",
	"coverage/lcov.info", "coverage/tmp/raw.json", "coverage/report.raw",
	"src/__pycache__/util.cpython-311.pyc", ".venv/bin/activate",
	"vendor/modules.txt", "vendor/licensed/notice.txt",
	"apps/web/src/app.tsx", "apps/web/node_modules/react/index.js",
	"apps/web/dist/index.html", "apps/web/build/robots.txt", "apps/web/build/asset.js",
	"apps/api/src/main.py", "apps/api/dist/main.js",
	"packages/ui/dist/index.js", "packages/ui/dist/style.css",
	"packages/config/dist/index.js",
	"packages/api/src/secret.env", "packages/api/src/handler.go",
	"packages/api/node_modules/dep/index.js",
	"assets/logo.png", "assets/icons/menu.svg", "assets/source/logo.psd",
	"assets/icons/thumbs/menu.png", "assets/manifest.json",
	"scripts/build.sh", "docs/guide.md", "app.log",
}

// TestFilterFSRealWorldPatternOracle runs realistic, multi-folder ignore lists
// that alternate excludes and re-includes several times, and asserts — via an
// independent oracle — that the pruned walk keeps exactly the right files. This
// is the core invariant: pruning is an optimization and must never change which
// files survive, no matter how complex the pattern interleaving.
func TestFilterFSRealWorldPatternOracle(t *testing.T) {
	excludeCases := []struct {
		name    string
		exclude []string
	}{
		{"node build output, keep artifact+config", []string{"node_modules", "dist", "coverage", "!dist/keep.js", "!coverage/lcov.info"}},
		{"next.js caches, keep public config", []string{"node_modules", ".next", "build", "!.next/static/**", "!build/manifest.json"}},
		{"python bytecode/venv/build, keep stub", []string{"**/__pycache__", "**/*.pyc", ".venv", "build", "dist", "!src/generated/**", "!dist/wheel-metadata.json"}},
		{"allowlist src+manifest, re-drop generated", []string{"**", "!src/**", "!package.json", "!README.md", "src/generated/**"}},
		{"go allowlist minus tests", []string{"**", "!**/*.go", "!go.mod", "!go.sum", "**/*_test.go"}},
		{"monorepo node_modules+dist, keep shared dist", []string{"apps/*/node_modules", "apps/*/dist", "packages/*/node_modules", "packages/*/dist", "!packages/ui/dist/**", "!packages/config/dist/index.js"}},
		{"vendor dir vs dist contents, keep licensed", []string{"vendor", "dist/**", "!dist/.gitkeep", "!vendor/licensed/notice.txt"}},
		{"deep alternation apps/web/build", []string{"apps", "!apps/web/**", "apps/web/build/**", "!apps/web/build/robots.txt"}},
		{"four-level alternation to a file", []string{"packages/**", "!packages/api/**", "packages/api/node_modules/**", "!packages/api/src/**", "packages/api/src/secret.env"}},
		{"docker build context, keep prod lockfile", []string{".git", ".github", "docs", "coverage", "**/*.log", "node_modules", "dist", "!package-lock.json"}},
		{"assets images minus psd/thumbs", []string{"**", "!assets/**/*.png", "!assets/**/*.svg", "!assets/manifest.json", "assets/**/*.psd", "assets/**/thumbs/**"}},
		{"terraform state, keep lockfile+example", []string{".terraform", "**/*.tfstate", "**/*.tfstate.backup", "!.terraform.lock.hcl", "!examples/dev.tfvars"}},
		{"multilang monorepo cleanup", []string{"**/node_modules", "**/__pycache__", "**/*.pyc", ".venv", "build", "dist", "!apps/web/dist/index.html", "!packages/ui/dist/**", "!scripts/**"}},
		{"ship src+generated, re-exclude secrets", []string{"**", "!src/**", "!packages/ui/dist/**", "src/generated/private/**", "!src/generated/private/schema.json"}},
		{"coverage reports allowlist", []string{"**", "!coverage/**", "coverage/tmp/**", "coverage/*.raw", "!coverage/lcov.info"}},
	}

	for _, c := range excludeCases {
		t.Run("exclude/"+c.name, func(t *testing.T) {
			root := t.TempDir()
			writeTree(t, root, realWorldTree...)
			keptFiles, _, _ := walkCase(t, root, nil, c.exclude)
			require.ElementsMatch(t, oracleKept(t, realWorldTree, nil, c.exclude), keptFiles,
				"pruned walk kept a different set than the per-file oracle")
		})
	}

	// Include + exclude together, across different folders.
	comboCases := []struct {
		name             string
		include, exclude []string
	}{
		{"go sources minus tests, drop vendor", []string{"**/*.go", "!**/*_test.go"}, []string{"vendor", "!vendor/licensed/**"}},
		{"assets minus thumbs, drop source dir", []string{"assets/**"}, []string{"**/thumbs/**", "assets/source"}},
		{"webp/png everywhere, prune node_modules", []string{"**/*.png", "**/*.svg"}, []string{"**/node_modules"}},
		{"src tree include, exclude generated privates", []string{"src/**"}, []string{"src/generated/private/**", "!src/generated/private/schema.json"}},
	}

	for _, c := range comboCases {
		t.Run("combo/"+c.name, func(t *testing.T) {
			root := t.TempDir()
			writeTree(t, root, realWorldTree...)
			keptFiles, _, _ := walkCase(t, root, c.include, c.exclude)
			require.ElementsMatch(t, oracleKept(t, realWorldTree, c.include, c.exclude), keptFiles,
				"pruned walk kept a different set than the per-file oracle")
		})
	}
}

// TestFilterFSRealWorldPruning spot-checks that, beyond staying correct, the
// walk actually prunes the big excluded directories in a few of the real-world
// cases (the performance goal).
func TestFilterFSRealWorldPruning(t *testing.T) {
	cases := []struct {
		name       string
		exclude    []string
		prunedDirs []string
	}{
		{"top-level node_modules pruned", []string{"node_modules", "dist"}, []string{"node_modules"}},
		{"nested node_modules pruned (monorepo)", []string{"**/node_modules"}, []string{"apps/web/node_modules", "packages/api/node_modules"}},
		{"contents-form dist pruned", []string{"apps/api/dist/**"}, []string{"apps/api/dist"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := t.TempDir()
			writeTree(t, root, realWorldTree...)
			_, _, visited := walkCase(t, root, nil, c.exclude)
			for _, d := range c.prunedDirs {
				require.False(t, descendedInto(visited, d), "expected %s to be pruned", d)
			}
		})
	}
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
