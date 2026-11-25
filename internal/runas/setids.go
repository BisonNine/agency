// Agency
// License: MIT

package runas

import (
	"fmt"
	"syscall"
)

// setIDs gives every thread of the process the account's groups, group and
// user, in that order (the user last, since it gives up the right to change
// the others), and checks that root cannot be regained.
func setIDs(a *Account) error {
	groups := a.Groups
	if len(groups) == 0 {
		groups = []int{a.GID}
	}
	if err := syscall.Setgroups(groups); err != nil {
		return fmt.Errorf("setgroups: %w", err)
	}
	if err := syscall.Setgid(a.GID); err != nil {
		return fmt.Errorf("setgid %d: %w", a.GID, err)
	}
	if err := syscall.Setuid(a.UID); err != nil {
		return fmt.Errorf("setuid %d: %w", a.UID, err)
	}
	if syscall.Geteuid() != a.UID || syscall.Getuid() != a.UID || syscall.Getegid() != a.GID {
		return fmt.Errorf("still running as uid %d gid %d", syscall.Geteuid(), syscall.Getegid())
	}
	if err := syscall.Setuid(0); err == nil {
		return fmt.Errorf("root could be regained after setuid %d", a.UID)
	}
	return nil
}
