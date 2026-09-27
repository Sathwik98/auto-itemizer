// Package layout checks the folder rules of IMPLEMENTATION_PLAN.md §2 by
// reading the imports of every package under internal/. It has only tests.
package layout

import (
	"errors"
	"go/build"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const module = "auto-itemizer/internal/"

// pkg is one package under internal/ with the imports of its code and tests.
type pkg struct {
	path    string // relative to internal/, e.g. "receipt/service"
	imports []string
}

// packages reads every package under internal/, which is this folder's parent.
func packages(t *testing.T) []pkg {
	t.Helper()
	var pkgs []pkg
	err := filepath.WalkDir("..", func(dir string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return err
		}
		p, err := build.ImportDir(dir, 0)
		var noGo *build.NoGoError
		if errors.As(err, &noGo) {
			return nil // a folder with only subfolders, like internal/receipt
		}
		if err != nil {
			return err
		}
		rel, err := filepath.Rel("..", dir)
		if err != nil {
			return err
		}
		pkgs = append(pkgs, pkg{
			path:    filepath.ToSlash(rel),
			imports: slices.Concat(p.Imports, p.TestImports, p.XTestImports),
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(pkgs) < 10 {
		t.Fatalf("found %d packages under internal/, want all of them", len(pkgs))
	}
	return pkgs
}

// A feature's SQL (<feature>/repository) may only be imported by packages of
// the same feature. This keeps the rule of ARCHITECTURE.md §1 that each service
// writes only its own tables: the SQL functions are public, so the compiler
// no longer checks it.
func TestRepositoryIsUsedOnlyByItsFeature(t *testing.T) {
	for _, p := range packages(t) {
		for _, imp := range p.imports {
			rest, ok := strings.CutPrefix(imp, module)
			if !ok {
				continue
			}
			feature, layer, _ := strings.Cut(rest, "/")
			if layer == "repository" && p.path != feature && !strings.HasPrefix(p.path, feature+"/") {
				t.Errorf("%s imports %s: only %s/ may use its repository", p.path, imp, feature)
			}
		}
	}
}

// Core packages hold types and pure rules (ARCHITECTURE.md §6), so they import
// no database code.
func TestCoreHasNoDatabaseCode(t *testing.T) {
	database := []string{module + "db", "database/sql", "modernc.org/sqlite"}
	for _, p := range packages(t) {
		if !strings.Contains("/"+p.path+"/", "/core/") {
			continue
		}
		for _, imp := range p.imports {
			if slices.Contains(database, imp) {
				t.Errorf("%s imports %s: core packages have no database code", p.path, imp)
			}
		}
	}
}
