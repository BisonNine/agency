// ClawEh
// License: MIT

package agent

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// concreteAgent is the SDK's concrete agent type, named nowhere in the host.
var concreteAgent = regexp.MustCompile(`\bloop\.Agent\b`)

// The host reaches the SDK agent only through the sdkagent.Agent interface:
// the SDK root is imported as sdkagent, the concrete *loop.Agent is never
// mentioned, and only turn_agent.go builds an agent (sdkagent.New).
func TestSDKAgentImportGuard(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		if concreteAgent.Match(src) {
			t.Errorf("%s mentions loop.Agent; hold the sdkagent.Agent interface", name)
		}
		sdkName := ""
		for _, imp := range f.Imports {
			if imp.Path.Value != `"go.bison9.com/agent"` {
				continue
			}
			sdkName = "agent"
			if imp.Name != nil {
				sdkName = imp.Name.Name
			}
			if sdkName != "sdkagent" {
				t.Errorf("%s imports go.bison9.com/agent without the alias sdkagent", name)
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			if sdkName != "" && pkg.Name == sdkName && sel.Sel.Name == "New" && name != "turn_agent.go" {
				t.Errorf("%s calls the SDK's New; only turn_agent.go builds an agent", fset.Position(sel.Pos()))
			}
			return true
		})
	}
}
