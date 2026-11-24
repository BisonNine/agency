package install

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.bison9.com/agency/global"
)

// blockPath puts a regular file at the host path p, so creating a folder at
// or under it fails.
func blockPath(t *testing.T, h *host, p string) {
	t.Helper()
	writeHostFile(t, h, p, "not a folder", 0o600)
}

// When a bin folder cannot be created, resolveBinDir says which one.
func TestResolveBinDir_CreationFailures(t *testing.T) {
	alice := func(root bool) *TargetUser { return &TargetUser{HomeDir: "/home/alice", IsRoot: root} }
	cases := []struct {
		name     string
		block    string
		tu       *TargetUser
		custom   string
		existing *ExistingInstall
		wantIn   string
	}{
		{name: "custom", block: "/srv", tu: alice(false), custom: "/srv/tools", wantIn: "/srv/tools"},
		{name: "system", block: "/usr/local", tu: alice(true), wantIn: "/usr/local/bin"},
		{name: "user", block: "/home/alice/.local", tu: alice(false), wantIn: "/home/alice/.local/bin"},
		{
			name: "opt claw from old binary", block: "/opt/agency/bin", tu: alice(true),
			existing: &ExistingInstall{BinaryPath: "/opt/agency/claw"}, wantIn: "/opt/agency/bin",
		},
		{
			name: "opt claw home", block: "/opt/agency/bin", tu: alice(true),
			existing: &ExistingInstall{AgencyHome: "/opt/agency"}, wantIn: "/opt/agency/bin",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := fakeHost(t)
			blockPath(t, h, tc.block)
			got, err := h.resolveBinDir(tc.tu, tc.custom, tc.existing)
			if err == nil {
				t.Fatalf("resolveBinDir() = %q, nil, want an error", got)
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("error %q does not name %s", err, tc.wantIn)
			}
		})
	}
}

// A detected binary whose folder is gone is not reused: the default for the
// mode applies.
func TestResolveBinDir_ExistingFolderGone(t *testing.T) {
	h := fakeHost(t)
	got, err := h.resolveBinDir(&TargetUser{HomeDir: "/home/alice", IsRoot: true}, "",
		&ExistingInstall{BinaryPath: "/usr/local/sbin/claw"})
	if err != nil || got != "/usr/local/bin" {
		t.Fatalf("resolveBinDir() = %q, %v, want /usr/local/bin", got, err)
	}
}

// A binary in a folder next to /opt/agency is not an /opt/agency install: its
// folder is kept as it is.
func TestResolveBinDir_SiblingOfOptAgency(t *testing.T) {
	h := fakeHost(t)
	if err := os.MkdirAll(h.path("/opt/agency-old"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := h.resolveBinDir(&TargetUser{HomeDir: "/home/alice", IsRoot: true}, "",
		&ExistingInstall{BinaryPath: "/opt/agency-old/claw"})
	if err != nil || got != "/opt/agency-old" {
		t.Fatalf("resolveBinDir() = %q, %v, want /opt/agency-old", got, err)
	}
}

// ensureOptAgencyBinDir leaves an existing /opt/agency/bin in place.
func TestEnsureOptAgencyBinDir_Existing(t *testing.T) {
	h := fakeHost(t)
	writeHostFile(t, h, "/opt/agency/bin/claw", "x", 0o700)
	got, err := h.ensureOptAgencyBinDir()
	if err != nil || got != optAgencyBinDir {
		t.Fatalf("ensureOptAgencyBinDir() = %q, %v", got, err)
	}
	if !h.fileExists("/opt/agency/bin/claw") {
		t.Error("the binary in /opt/agency/bin was removed")
	}
}

// resolveAgencyHome: the env, then the detected install, then /opt/agency when the
// binary goes there or the folder exists, else ~/.agency.
func TestResolveAgencyHome_Fallbacks(t *testing.T) {
	t.Setenv(global.EnvVarHome, "")
	tu := &TargetUser{HomeDir: "/home/alice"}
	home := filepath.Join("/home/alice", global.DefaultDataDir)

	h := fakeHost(t)
	if got := h.resolveAgencyHome(tu, "/home/alice/bin", nil); got != home {
		t.Errorf("no install, no /opt/agency = %q, want %q", got, home)
	}
	if got := h.resolveAgencyHome(tu, "/home/alice/bin", &ExistingInstall{BinaryPath: "/opt/agency-old/claw"}); got != home {
		t.Errorf("binary next to /opt/agency = %q, want %q", got, home)
	}
	if got := h.resolveAgencyHome(tu, "/home/alice/bin", &ExistingInstall{}); got != home {
		t.Errorf("empty install = %q, want %q", got, home)
	}
	if err := os.MkdirAll(h.path("/opt/agency"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := h.resolveAgencyHome(tu, "/home/alice/bin", nil); got != "/opt/agency" {
		t.Errorf("/opt/agency present = %q, want /opt/agency", got)
	}
}
