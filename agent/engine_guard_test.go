// ClawEh
// License: MIT

package agent

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// engineFilePrefix names the turn engine's files: the generic turn loop that
// phase 5c moves to the SDK unchanged. Everything else in the package is the
// host.
const engineFilePrefix = "engine_"

// engineImportAllowed reports whether an engine file may import path: the
// standard library and the SDK, nothing of the host and no third-party
// module.
func engineImportAllowed(path string) bool {
	if first, _, _ := strings.Cut(path, "/"); !strings.Contains(first, ".") {
		return true // standard library
	}
	return strings.HasPrefix(path, "go.bison9.com/agent/")
}

// engineFiles returns the engine's files, parsed: its non-test files, and
// its tests too with tests set.
func engineFiles(t *testing.T, fset *token.FileSet, tests bool) []*ast.File {
	t.Helper()
	names, err := filepath.Glob(engineFilePrefix + "*.go")
	if err != nil {
		t.Fatal(err)
	}
	var files []*ast.File
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") && !tests {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	if len(files) == 0 {
		t.Fatal("no engine files found")
	}
	return files
}

// The engine imports only the standard library and the SDK.
func TestEngineImportsOnlySDK(t *testing.T) {
	fset := token.NewFileSet()
	for _, f := range engineFiles(t, fset, true) {
		for _, imp := range f.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatal(err)
			}
			if !engineImportAllowed(path) {
				t.Errorf("%s imports %s: an engine file may import only the standard library and go.bison9.com/agent/...",
					fset.Position(imp.Pos()).Filename, path)
			}
		}
	}
}

// The engine files, and with them their tests, type-check on their own, as a
// package of just them: they use no identifier the host files of this
// package declare, so they can be moved to a package of their own unchanged.
func TestEngineStandsAlone(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the SDK's export data")
	}
	fset := token.NewFileSet()
	files := engineFiles(t, fset, true)

	var imports []string
	for _, f := range files {
		for _, imp := range f.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Contains(imports, path) {
				imports = append(imports, path)
			}
		}
	}
	exports := exportData(t, imports)
	lookup := func(path string) (io.ReadCloser, error) {
		file, ok := exports[path]
		if !ok || file == "" {
			return nil, fmt.Errorf("no export data for %s", path)
		}
		return os.Open(file)
	}

	var problems []string
	conf := types.Config{
		Importer: importer.ForCompiler(fset, "gc", lookup),
		Error:    func(err error) { problems = append(problems, err.Error()) },
	}
	if _, err := conf.Check("engine", fset, files, nil); err != nil && len(problems) == 0 {
		problems = append(problems, err.Error())
	}
	for _, p := range problems {
		t.Errorf("engine file does not stand alone: %s", p)
	}
}

// exportData maps every package imports need, themselves included, to its
// compiled export data.
func exportData(t *testing.T, imports []string) map[string]string {
	t.Helper()
	args := append([]string{"list", "-export", "-deps", "-f", "{{.ImportPath}}\t{{.Export}}"}, imports...)
	cmd := exec.Command("go", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, stderr.String())
	}
	exports := map[string]string{}
	for line := range strings.Lines(string(out)) {
		path, file, _ := strings.Cut(strings.TrimSpace(line), "\t")
		exports[path] = file
	}
	return exports
}
