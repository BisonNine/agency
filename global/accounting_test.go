// Agency
// License: MIT
//
// Copyright (c) 2026 Tenebris Technologies Inc.

package global

import "testing"

func TestNoOwnerIdentity(t *testing.T) {
	id := NoOwnerIdentity("acme")
	if id.Agent != NoOwnerAgent || id.Project != "acme" || id.Name == "" {
		t.Errorf("NoOwnerIdentity = %+v", id)
	}
	if DefaultBilling != "default" || MemoryBilling != "memory" {
		t.Errorf("codes = %q, %q", DefaultBilling, MemoryBilling)
	}
}
