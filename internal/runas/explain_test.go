// Agency
// License: MIT

package runas

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// TestExplain: a permission error on a path, however wrapped, becomes one
// sentence naming the account and the path; anything else is unchanged.
func TestExplain(t *testing.T) {
	acct := &Account{Name: "agency"}
	dir := t.TempDir()
	existing := filepath.Join(dir, "backup.tar.gz")
	if err := os.WriteFile(existing, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	perm := func(op, path string) error {
		return fmt.Errorf("restore: open %s: %w", path, &fs.PathError{Op: op, Path: path, Err: syscall.EACCES})
	}
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"read an archive", perm("open", "/home/alice/backup.tar.gz"), "The agency account cannot read /home/alice/backup.tar.gz."},
		{"read an existing file", perm("open", existing), "The agency account cannot read " + existing + "."},
		{"create a file", perm("open", filepath.Join(dir, "new.tar.gz")), "The agency account cannot write to " + filepath.Join(dir, "new.tar.gz") + "."},
		{"make a folder", perm("mkdir", "/root/backups"), "The agency account cannot write to /root/backups."},
		{"a temporary file", perm("mkdirtemp", "/root/backups/.agency-backup-*"), "The agency account cannot write to /root/backups."},
		{"rename", fmt.Errorf("x: %w", &os.LinkError{Op: "rename", Old: "/a", New: "/root/b", Err: syscall.EPERM}), "The agency account cannot write to /root/b."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Explain(tc.err, acct); got == nil || got.Error() != tc.want {
				t.Errorf("Explain = %v, want %q", got, tc.want)
			}
		})
	}

	plain := errors.New("restore: not a gzip archive")
	notPath := fmt.Errorf("x: %w", fs.ErrPermission)
	notPerm := &fs.PathError{Op: "open", Path: "/x", Err: syscall.ENOENT}
	for _, err := range []error{plain, notPath, notPerm} {
		if got := Explain(err, acct); got != err { //nolint:errorlint // identity
			t.Errorf("Explain(%v) = %v, want it unchanged", err, got)
		}
	}
	if got := Explain(perm("open", "/x"), nil); got.Error() == "The agency account cannot read /x." {
		t.Error("rewritten without a switch")
	}
	if Explain(nil, acct) != nil {
		t.Error("nil not kept")
	}
}
