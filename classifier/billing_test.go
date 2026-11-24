// Agency
// License: MIT
//
// Copyright (c) 2026 Tenebris Technologies Inc.

package classifier

import (
	"context"
	"testing"
)

// A classifier is billed to the code of the work that created it, written by
// Agency like its owner: neither the definition nor an update can set it,
// and an update keeps it. Its model calls carry it.
func TestClassifierBilling(t *testing.T) {
	e := newEnv(t)
	e.scope.Billing = "acme"
	def := definition()
	def["billing"] = "chosen-by-the-agent"
	if got := e.create(def); got.Billing != "acme" {
		t.Fatalf("created billed %q, want acme", got.Billing)
	}
	e.scope.Billing = "garden"
	if err := e.svc.Update(context.Background(), e.scope, "inbox-priority", []byte(`{"billing": "patched", "dry_run": true}`)); err != nil {
		t.Fatalf("Update: %v", err)
	}
	d, err := e.svc.validDefinition(e.scope, "inbox-priority")
	if err != nil {
		t.Fatal(err)
	}
	if d.Billing != "acme" || !d.DryRun {
		t.Fatalf("updated billed %q (dry run %v), want acme kept", d.Billing, d.DryRun)
	}
	e.run("inbox-priority")
	e.model.mu.Lock()
	billings := append([]string(nil), e.model.billings...)
	e.model.mu.Unlock()
	if len(billings) == 0 {
		t.Fatal("no model call")
	}
	for _, b := range billings {
		if b != "acme" {
			t.Errorf("model call billed %q, want acme", b)
		}
	}
}

// The billing code is not in the published schema: Agency writes it.
func TestClassifierBillingNotInSchema(t *testing.T) {
	props, ok := DefinitionSchema()["properties"].(map[string]any)
	if !ok {
		t.Fatal("the definition schema has no properties")
	}
	if _, has := props["billing"]; has {
		t.Error("the definition schema offers billing")
	}
}
