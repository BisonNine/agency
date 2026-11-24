// Agency
// License: MIT

package config

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// config is plain data: of the agent SDK it may import only the packages
// that are plain data themselves, so loading a config never pulls in a
// provider client or the context engine. Add a package here on purpose.
var allowedSDKImports = []string{
	"go.bison9.com/agent/modelspec",
}

func TestConfigImportsOnlyPlainDataSDKPackages(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range parsed.Imports {
			name, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatal(err)
			}
			if name != "go.bison9.com/agent" && !strings.HasPrefix(name, "go.bison9.com/agent/") {
				continue
			}
			if !slices.Contains(allowedSDKImports, name) {
				t.Errorf("%s imports %s; config may import only %v of the SDK", path, name, allowedSDKImports)
			}
		}
	}
}
