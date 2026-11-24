// Agency
// License: MIT
//
// Copyright (c) 2026 Tenebris Technologies Inc.

package forum

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestReadSourceOpenFailure(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "doc.md")
	writeTestFile(t, file, "x")
	p := &preflight{env: preflightEnv{
		ReadAllowed: func(string) error { return nil },
		OpenFile:    func(string) (*os.File, error) { return nil, errors.New("sandbox refused") },
	}}
	_, err := p.readSource(file)
	wantErrContains(t, "open refused", err, "cannot be read", "sandbox refused")
}

func TestReadableUnresolvable(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "doc.md")
	writeTestFile(t, file, "x")
	p := &preflight{env: preflightEnv{ReadAllowed: func(string) error { return nil }}}
	_, _, err := p.readable(filepath.Join(file, "below"))
	wantErrContains(t, "path below a file", err, "cannot be resolved")
}

func TestReadRegularFailures(t *testing.T) {
	dir := t.TempDir()
	d, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer edgeClose(t, d)
	_, err = readRegular(d)
	wantErrContains(t, "directory", err, "is not a regular file")

	file := filepath.Join(dir, "doc.md")
	writeTestFile(t, file, "x")
	w, err := os.OpenFile(file, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer edgeClose(t, w)
	_, err = readRegular(w)
	wantErrContains(t, "write-only file", err, "cannot be read")

	closed, err := os.Open(file)
	if err != nil {
		t.Fatal(err)
	}
	edgeClose(t, closed)
	_, err = readRegular(closed)
	wantErrContains(t, "closed file", err, "cannot be read")
}

func TestIssueTextOfPlainError(t *testing.T) {
	if got := issueText(errors.New("plain")); got != "plain" {
		t.Errorf("issueText = %q", got)
	}
}
