// Agency
// License: MIT
//
// Copyright (c) 2026 Tenebris Technologies Inc.

package tools

import (
	"context"
	"testing"

	"go.bison9.com/agency/global"
)

// Each WithBilling is a code of its own: a change to it reaches the work on
// it and its records, never the code it was made from.
func TestInheritedBilling(t *testing.T) {
	ctx := context.Background()
	if _, ok := Billing(ctx); ok || InheritedBillingOf(ctx) != nil {
		t.Fatal("a bare context has an inherited code")
	}
	parent := WithBilling(ctx, "acme")
	child := WithBilling(parent, "acme")
	var told []string
	InheritedBillingOf(child).OnSet(func(code string) { told = append(told, code) })
	InheritedBillingOf(child).Set("acme-2")
	if code, _ := Billing(child); code != "acme-2" {
		t.Errorf("child = %q, want acme-2", code)
	}
	if code, _ := Billing(parent); code != "acme" {
		t.Errorf("parent = %q, want acme kept", code)
	}
	if len(told) != 1 || told[0] != "acme-2" {
		t.Errorf("records told %v", told)
	}
	// The same run's code on a context built anew.
	same := WithInheritedBilling(ctx, InheritedBillingOf(child))
	if code, _ := Billing(same); code != "acme-2" {
		t.Errorf("same run = %q", code)
	}
	if _, ok := Billing(WithBilling(child, "")); ok {
		t.Error("WithBilling(\"\") kept an inherited code")
	}
	if _, ok := Billing(WithInheritedBilling(child, nil)); ok {
		t.Error("WithInheritedBilling(nil) kept an inherited code")
	}
}

func TestToolDepsBillingOf(t *testing.T) {
	ctx := context.Background()
	if got := (ToolDeps{}).BillingOf(ctx); got != global.DefaultBilling {
		t.Errorf("no resolver = %q", got)
	}
	if got := (ToolDeps{}).BillingOf(WithBilling(ctx, "acme")); got != "acme" {
		t.Errorf("inherited = %q", got)
	}
	d := ToolDeps{Billing: func(context.Context) string { return "garden" }}
	if got := d.BillingOf(ctx); got != "garden" {
		t.Errorf("resolver = %q", got)
	}
}

func TestCheckedBilling(t *testing.T) {
	for _, tc := range []struct{ code, want string }{
		{"acme", "acme"},
		{"", global.DefaultBilling},
		{"not a code", global.DefaultBilling},
	} {
		if got := CheckedBilling(tc.code, "test"); got != tc.want {
			t.Errorf("CheckedBilling(%q) = %q, want %q", tc.code, got, tc.want)
		}
	}
}
