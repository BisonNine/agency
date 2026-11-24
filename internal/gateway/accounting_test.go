// Agency
// License: MIT
//
// Copyright (c) 2026 Tenebris Technologies Inc.

package gateway

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.bison9.com/agent/accounting"

	"go.bison9.com/agency/config"
	"go.bison9.com/agency/logger"
)

// report records one call of model with tokens and no cost of its own.
func report(model string) {
	accounting.Report(accounting.Record{
		Time: time.Now(), Agent: "alice", Project: "acme", Kind: accounting.KindText, Model: model,
		InputTokens: accounting.KnownInt(1_000_000), OutputTokens: accounting.KnownInt(0), Success: true,
	})
}

// readRecords stops the sink and returns the records the log holds.
func readRecords(t *testing.T, path string) []map[string]any {
	t.Helper()
	closeAccounting()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(string(data)), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("not a record: %q", line)
		}
		out = append(out, rec)
	}
	return out
}

// The accounting log is <home>/logs/accounting.log with the logs area's
// modes; the pricing table is <home>/pricing.json when it exists, a bad one
// keeping the table in force.
func TestAccountingLog(t *testing.T) {
	buf := &lockedBuf{}
	t.Cleanup(logger.RedirectForTest(buf))
	t.Cleanup(func() {
		accounting.ClearPricing()
		accounting.SetLogger(nil)
		closeAccounting()
	})
	home := t.TempDir()
	cfg := &config.Config{}
	cfg.Security.Permissions = config.DefaultPermissions()
	cfg.Security.Permissions.Logs = config.AreaPermissions{DirMode: "0750", FileMode: "0640"}
	pricing := filepath.Join(home, pricingFileName)
	if err := os.WriteFile(pricing, []byte(`{"prices": [{"model": "m1", "input": 2}]}`), 0o600); err != nil {
		t.Fatal(err)
	}

	startAccounting(cfg, home)
	path := filepath.Join(home, "logs", accounting.FileName)
	for p, want := range map[string]os.FileMode{path: 0o640, filepath.Dir(path): 0o750 | os.ModeDir} {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode() != want {
			t.Errorf("%s mode = %v, want %v", p, info.Mode(), want)
		}
	}
	report("m1")
	recs := readRecords(t, path)
	if len(recs) != 1 || recs[0]["project"] != "acme" || recs[0]["cost_source"] != string(accounting.CostEstimated) {
		t.Fatalf("records = %v, want one acme record priced from the table", recs)
	}

	// A bad file on reload keeps the table and says so once.
	if err := os.WriteFile(pricing, []byte(`{"prices": [{"model": "m1", "inputs": 2}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	loadPricing(home)
	if n := strings.Count(buf.String(), "Pricing table not loaded"); n != 1 {
		t.Errorf("%d error lines for a bad pricing file, want 1", n)
	}
	// No file: nothing changes, nothing is logged.
	if err := os.Remove(pricing); err != nil {
		t.Fatal(err)
	}
	loadPricing(home)
	if err := accounting.Start(filepath.Dir(path)); err != nil {
		t.Fatal(err)
	}
	report("m1")
	recs = readRecords(t, path)
	if last := recs[len(recs)-1]; last["cost_source"] != string(accounting.CostEstimated) {
		t.Errorf("after a bad file the table was dropped: %v", last)
	}
	if n := strings.Count(buf.String(), "Pricing table not loaded"); n != 1 {
		t.Errorf("a missing pricing file was reported")
	}
}

// A sink that cannot start is logged and does not stop the gateway.
func TestAccountingLog_StartFails(t *testing.T) {
	buf := &lockedBuf{}
	t.Cleanup(logger.RedirectForTest(buf))
	t.Cleanup(func() { accounting.SetLogger(nil) })
	home := t.TempDir()
	// logs is a file, so the directory cannot be made.
	if err := os.WriteFile(filepath.Join(home, "logs"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	startAccounting(&config.Config{}, home)
	if !strings.Contains(buf.String(), "Accounting log not started") {
		t.Errorf("no error line: %s", buf.String())
	}
	closeAccounting()
}
