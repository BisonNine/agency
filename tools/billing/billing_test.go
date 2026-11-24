// Agency
// License: MIT
//
// Copyright (c) 2026 Tenebris Technologies Inc.

package billing

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"go.bison9.com/agent/accounting"
	"go.bison9.com/agent/toolspec"

	"go.bison9.com/agency/config"
	"go.bison9.com/agency/tools"
)

// fakeBilling is a session's billing code with the tool's two closures.
type fakeBilling struct {
	code    string
	session string
	err     error
}

func (f *fakeBilling) get(context.Context) string { return f.code }

func (f *fakeBilling) set(_ context.Context, sessionKey, code string) error {
	if f.err != nil {
		return f.err
	}
	if err := accounting.CheckProject(code); err != nil {
		return err
	}
	f.code, f.session = code, sessionKey
	return nil
}

func call(def toolspec.ToolDefinition, args map[string]any) *toolspec.Result {
	res, err := def.Handler(&toolspec.ToolCall{Ctx: context.Background(), Args: args, AgentID: "alice", Session: "agent:alice:main"})
	if err != nil {
		panic(err)
	}
	return res
}

func TestBillingTool(t *testing.T) {
	tests := []struct {
		name     string
		args     map[string]any
		setErr   error
		want     string
		isError  bool
		wantCode string
	}{
		{"read", nil, nil, "Billing code: default", false, "default"},
		{"read with null", map[string]any{"code": nil}, nil, "Billing code: default", false, "default"},
		{"set", map[string]any{"code": "pentest-acme"}, nil, "Billing code set to pentest-acme.", false, "pentest-acme"},
		{"set with dots and underscores", map[string]any{"code": "Acme_2026.q4"}, nil, "Billing code set to Acme_2026.q4.", false, "Acme_2026.q4"},
		{"empty", map[string]any{"code": ""}, nil, invalidText, true, "default"},
		{"space", map[string]any{"code": "acme corp"}, nil, invalidText, true, "default"},
		{"too long", map[string]any{"code": fmt.Sprintf("%065d", 0)}, nil, invalidText, true, "default"},
		{"not a string", map[string]any{"code": 42.0}, nil, invalidText, true, "default"},
		{"refused by the host", map[string]any{"code": "acme"}, fmt.Errorf("check: %w", accounting.ErrInvalidProject), invalidText, true, "default"},
		{"store failed", map[string]any{"code": "acme"}, errors.New("disk full"), failedText, true, "default"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeBilling{code: "default", err: tt.setErr}
			res := call(definition(f.get, f.set), tt.args)
			if res.ForLLM != tt.want || res.IsError != tt.isError {
				t.Fatalf("result = %q (error %v), want %q (error %v)", res.ForLLM, res.IsError, tt.want, tt.isError)
			}
			if f.code != tt.wantCode {
				t.Errorf("code = %q, want %q", f.code, tt.wantCode)
			}
			if tt.wantCode != "default" && f.session != "agent:alice:main" {
				t.Errorf("set for session %q, want the call's", f.session)
			}
		})
	}
}

func TestBillingTool_NoSetter(t *testing.T) {
	f := &fakeBilling{code: "acme"}
	def := definition(f.get, nil)
	if res := call(def, nil); res.ForLLM != "Billing code: acme" {
		t.Errorf("read = %q", res.ForLLM)
	}
	if res := call(def, map[string]any{"code": "other"}); !res.IsError || res.ForLLM != unavailText {
		t.Errorf("set without a setter = %q (error %v)", res.ForLLM, res.IsError)
	}
}

func TestBillingTool_Definition(t *testing.T) {
	def := definition((&fakeBilling{}).get, nil)
	if def.Name != "billing" || !def.SessionScoped || len(def.Parameters) != 1 || def.Parameters[0].Name != "code" || def.Parameters[0].Required {
		t.Errorf("definition = %+v", def)
	}
}

func TestProvider(t *testing.T) {
	p := GlobalProvider
	if p.Namespace() != "billing" || p.Suite() != config.BillingSuite || p.Description() == "" {
		t.Errorf("provider metadata: %q %q %q", p.Namespace(), p.Suite(), p.Description())
	}
	if ok, _ := p.Available(nil); !ok {
		t.Error("provider not available")
	}
	if defs := p.RegisterTools(toolspec.Deps{}); len(defs) != 0 {
		t.Errorf("enumeration pass registered %d tools", len(defs))
	}
	if defs := p.RegisterTools(toolspec.Deps{Host: tools.ToolDeps{}}); len(defs) != 0 {
		t.Errorf("deps without a config registered %d tools", len(defs))
	}
	// Outside the agent loop the code is the inherited one, else the
	// default, and it cannot be changed.
	defs := p.RegisterTools(toolspec.Deps{Host: tools.ToolDeps{Cfg: &config.Config{}}})
	if len(defs) != 1 {
		t.Fatalf("registered %d tools, want 1", len(defs))
	}
	res, err := defs[0].Handler(&toolspec.ToolCall{Ctx: tools.WithBilling(context.Background(), "acme")})
	if err != nil || res.ForLLM != "Billing code: acme" {
		t.Errorf("inherited read = %+v, %v", res, err)
	}
	res, err = defs[0].Handler(&toolspec.ToolCall{Ctx: context.Background()})
	if err != nil || res.ForLLM != "Billing code: default" {
		t.Errorf("default read = %+v, %v", res, err)
	}
}
