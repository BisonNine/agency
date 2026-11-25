// Agency
// License: MIT

package runas

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// writeOps are the os operations that change a path (fs.PathError.Op).
var writeOps = map[string]bool{
	"mkdir": true, "mkdirat": true, "remove": true, "unlinkat": true, "write": true,
	"chmod": true, "chown": true, "lchown": true, "truncate": true, "symlink": true,
	"link": true, "chtimes": true, "rename": true,
}

// tempOps create a file or folder in the directory their pattern names.
var tempOps = map[string]bool{"mkdirtemp": true, "createtemp": true}

// Explain rewrites err, the error of a command that switched to acct, when it
// is a permission error on a path: the account cannot reach a file the
// operator named (an archive in their own home folder, a destination only
// root may write), and the command's own wording would not say why. It
// returns err unchanged otherwise, and always when acct is nil.
func Explain(err error, acct *Account) error {
	if err == nil || acct == nil || !errors.Is(err, fs.ErrPermission) {
		return err
	}
	if le, ok := errors.AsType[*os.LinkError](err); ok {
		return fmt.Errorf("The %s account cannot write to %s.", acct.Name, le.New) //nolint:staticcheck // a sentence for the operator
	}
	pe, ok := errors.AsType[*fs.PathError](err)
	if !ok {
		return err
	}
	path, verb := pe.Path, "read"
	switch {
	case tempOps[pe.Op]:
		path, verb = filepath.Dir(pe.Path), "write to"
	case writeOps[pe.Op]:
		verb = "write to"
	case pe.Op == "open" || pe.Op == "openat":
		// Opening a file that is not there failed: it was being created.
		if _, derr := os.Lstat(filepath.Dir(path)); derr == nil {
			if _, ferr := os.Lstat(path); errors.Is(ferr, fs.ErrNotExist) {
				verb = "write to"
			}
		}
	}
	return fmt.Errorf("The %s account cannot %s %s.", acct.Name, verb, path) //nolint:staticcheck // a sentence for the operator
}
