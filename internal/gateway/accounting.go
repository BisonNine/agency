// Agency
// License: MIT
//
// Copyright (c) 2026 Tenebris Technologies Inc.

package gateway

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"go.bison9.com/agent/accounting"
	"go.bison9.com/agent/fsmode"

	"go.bison9.com/agency/config"
	"go.bison9.com/agency/logger"
)

// pricingFileName is the optional pricing table in the data directory, from
// which the cost of a model call that reports none is estimated.
const pricingFileName = "pricing.json"

// startAccounting starts the record of every model call: one JSON line per
// call in <baseDir>/logs/accounting.log (accounting.FileName), created with
// the logs area's modes, and loads <baseDir>/pricing.json when it exists. A
// sink that cannot start is logged and does not stop the gateway: the
// records then go to the main log. closeAccounting stops it.
func startAccounting(cfg *config.Config, baseDir string) {
	accounting.SetLogger(logger.SDK())
	m := cfg.PermissionModes(config.AreaLogs)
	dir := filepath.Join(baseDir, "logs")
	if err := accounting.Start(dir, accounting.WithFileModes(fsmode.Modes{Dir: m.Dir, File: m.File})); err != nil {
		logger.ErrorCF("gateway", "Accounting log not started; model call records go to the main log",
			map[string]any{"path": filepath.Join(dir, accounting.FileName), "error": err.Error()})
	}
	loadPricing(baseDir)
}

// loadPricing installs <baseDir>/pricing.json as the pricing table when the
// file exists. A file that cannot be read or is not valid is logged once and
// the table in force is kept.
func loadPricing(baseDir string) {
	path := filepath.Join(baseDir, pricingFileName)
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err := accounting.LoadPricing(path); err != nil {
		logger.ErrorCF("gateway", "Pricing table not loaded; the previous one is kept",
			map[string]any{"path": path, "error": err.Error()})
		return
	}
	logger.InfoCF("gateway", "Pricing table loaded", map[string]any{"path": path})
}

// closeAccounting writes the records still queued and closes the
// accounting log.
func closeAccounting() {
	if err := accounting.Close(); err != nil {
		logger.WarnCF("gateway", "Accounting log close failed", map[string]any{"error": err.Error()})
	}
}
