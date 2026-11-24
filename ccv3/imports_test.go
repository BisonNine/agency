// Agency
// License: MIT
//
// Copyright (c) 2026 Tenebris Technologies Inc.

package ccv3

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The ccv3 package imports no Agency package but the stdlib-only
// internal/mergepatch, so every part of Agency can use it.
func TestCCv3ImportsNoClawEhPackage(t *testing.T) {
	const module = "go.bison9.com/agency/"
	allowed := map[string]bool{module + "internal/mergepatch": true}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range parsed.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(path, module) && !allowed[path] {
				t.Errorf("%s imports %s", f, path)
			}
		}
	}
}
