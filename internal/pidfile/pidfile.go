// Agency - process PID file
// License: MIT
//
// Copyright (c) 2026 Tenebris Technologies Inc.

// Package pidfile records the running Agency process id in its data
// directory, so a separate `agency` invocation can find the instance it belongs
// to and report on it.
//
// The data directory is the right scope. One binary runs several instances —
// a production service and a developer one on the same host — and a CLI command
// already resolves AGENCY_HOME to find the config. Looking the process up through
// systemd instead would need the command to know which unit it is, which it has
// no way to determine.
package pidfile

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"go.bison9.com/agency/fileutil"
	"go.bison9.com/agency/global"
	"go.bison9.com/agency/internal/perms"
	"go.bison9.com/agency/logger"
)

// Name is the file written into the data directory's internal/.
const Name = "agency.pid"

// Path returns the PID file path for a data directory.
func Path(dataDir string) string { return filepath.Join(dataDir, global.InternalDir, Name) }

// Write records the current process id. A failure is returned rather than
// fatal: the PID file is a convenience for reporting, and an instance that
// cannot write one should still start.
func Write(dataDir string) error {
	if dataDir == "" {
		return errors.New("pidfile: no data directory")
	}
	p := Path(dataDir)
	if err := os.MkdirAll(filepath.Dir(p), perms.SecretModes().Dir); err != nil {
		return fmt.Errorf("pidfile: create %s: %w", filepath.Dir(p), err)
	}
	if err := fileutil.WriteFileAtomic(p, []byte(strconv.Itoa(os.Getpid())+"\n"), perms.SecretModes().File); err != nil {
		return fmt.Errorf("pidfile: write %s: %w", p, err)
	}
	return nil
}

// Remove deletes the PID file. Missing is not an error — the point is that it
// is gone.
func Remove(dataDir string) {
	if dataDir == "" {
		return
	}
	p := Path(dataDir)
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		logger.WarnCF("gateway", "failed to remove PID file", map[string]any{"path": p, "error": err.Error()})
	}
}

// reloadMark is the PID file's second line once the process reloads its
// configuration on SIGHUP.
const reloadMark = "reload"

// MarkReloadable rewrites the PID file to say that this process reloads its
// configuration on SIGHUP. Call it only once the signal is handled: SIGHUP
// ends a process that does not handle it, so RequestReload signals only a
// process that has said it does.
func MarkReloadable(dataDir string) error {
	if dataDir == "" {
		return errors.New("pidfile: no data directory")
	}
	p := Path(dataDir)
	content := strconv.Itoa(os.Getpid()) + "\n" + reloadMark + "\n"
	if err := fileutil.WriteFileAtomic(p, []byte(content), perms.SecretModes().File); err != nil {
		return fmt.Errorf("pidfile: write %s: %w", p, err)
	}
	return nil
}

// RequestReload asks the running instance of a data directory to reload its
// configuration now. running says whether there is one; signalled whether it
// was asked (it is not when it runs a release that does not take the
// request, or the signal could not be sent).
func RequestReload(dataDir string) (running, signalled bool) {
	pid, running := Read(dataDir)
	if !running {
		return false, false
	}
	b, err := os.ReadFile(Path(dataDir))
	if err != nil {
		return true, false
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) < 2 || strings.TrimSpace(lines[1]) != reloadMark {
		return true, false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return true, false
	}
	return true, proc.Signal(syscall.SIGHUP) == nil
}

// Read returns the pid recorded for a data directory and whether that process
// is a LIVE Agency process.
//
// Both checks matter. A kill -9 leaves the file behind, and pids are recycled,
// so a file alone proves nothing: the process must still exist AND still run
// this program. Without the second check a status command would cheerfully report the
// memory of whatever unrelated program inherited the number.
func Read(dataDir string) (pid int, running bool) {
	if dataDir == "" {
		return 0, false
	}
	b, err := os.ReadFile(Path(dataDir))
	if err != nil {
		return 0, false
	}
	first, _, _ := strings.Cut(strings.TrimSpace(string(b)), "\n") // a second line is reloadMark
	pid, err = strconv.Atoi(strings.TrimSpace(first))
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, isAgency(pid)
}

// isAgency reports whether pid is alive and is an Agency process.
//
// Signal 0 tests for existence without delivering anything; it succeeds only
// for a process this user may signal. isOurExecutable then rejects a recycled
// pid now belonging to something else.
func isAgency(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if sigErr := proc.Signal(syscall.Signal(0)); sigErr != nil {
		return false
	}
	return isOurExecutable(pid)
}

// procRoot, selfExecutable and psOutput are replaced in tests.
var (
	procRoot       = "/proc"
	selfExecutable = os.Executable
	psOutput       = runPS
)

// psTimeout bounds one ps call.
const psTimeout = 5 * time.Second

// psCommand is the ps to run: on macOS the system's own, by its full path, so
// the PATH the process was started with does not matter.
func psCommand() string {
	if runtime.GOOS == "darwin" {
		return "/bin/ps"
	}
	return "ps"
}

// runPS returns one field of one process as ps prints it, without a header.
// It is how a process is inspected on a system without /proc (macOS). A pid
// that no longer exists is an *exec.ExitError.
func runPS(pid int, field string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), psTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, psCommand(), "-o", field+"=", "-p", strconv.Itoa(pid)).Output() //nolint:gosec // fixed command, a field name and a pid
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// hasProcfs reports whether this system has /proc at all.
func hasProcfs() bool {
	_, err := os.Stat(procRoot)
	return err == nil
}

// deletedSuffix is what Linux appends to /proc/<pid>/exe once the file has
// been replaced, as `upgrade` does to a running binary.
const deletedSuffix = " (deleted)"

// isOurExecutable reports whether pid runs this program, judged by its
// executable rather than the name it was started as, so a gateway started
// through the openclaw link or under another name is still recognised.
//
// /proc/<pid>/exe is the executable with every link resolved. It matches when
// it is the file this process runs, or a file with the installed binary's
// name (another build of it, such as the installed one seen from a
// development copy). Where exe cannot be read (another user's process), the
// command name is compared instead. On a system without /proc (macOS), the
// executable is taken from ps.
func isOurExecutable(pid int) bool {
	dir := filepath.Join(procRoot, strconv.Itoa(pid))
	self := ""
	if p, err := selfExecutable(); err == nil {
		self = strings.TrimSuffix(p, deletedSuffix)
		if resolved, err := filepath.EvalSymlinks(self); err == nil {
			self = resolved
		}
	}
	if exe, err := os.Readlink(filepath.Join(dir, "exe")); err == nil {
		exe = strings.TrimSuffix(exe, deletedSuffix)
		return exe == self || filepath.Base(exe) == global.InstalledBinaryName
	}
	comm, err := os.ReadFile(filepath.Join(dir, "comm")) //nolint:gosec // a /proc entry named by a pid
	if err != nil {
		if !hasProcfs() {
			return isOurExecutablePS(pid, self)
		}
		return true // the entry cannot be read: existence is the best available answer
	}
	name := strings.TrimSpace(string(comm))
	return name == global.InstalledBinaryName || (self != "" && name == commName(filepath.Base(self)))
}

// isOurExecutablePS is isOurExecutable for a system without /proc. ps prints
// the path the process was started by: it matches when it resolves to the
// file this process runs, or its name is the installed binary's or this
// binary's. When ps itself cannot be run, existence is the best available
// answer.
func isOurExecutablePS(pid int, self string) bool {
	path, err := psOutput(pid, "comm")
	if err != nil {
		var exit *exec.ExitError
		return !errors.As(err, &exit) // ps ran and found no such process
	}
	if path == "" {
		return false
	}
	if filepath.IsAbs(path) {
		if resolved, err := filepath.EvalSymlinks(path); err == nil {
			path = resolved
		}
		if path == self {
			return true
		}
	}
	name := filepath.Base(path)
	return name == global.InstalledBinaryName || (self != "" && name == filepath.Base(self))
}

// commName is name as /proc/<pid>/comm shows it: at most 15 bytes.
func commName(name string) string {
	if len(name) > 15 {
		return name[:15]
	}
	return name
}

// RSSBytes returns a process's resident set size — the physical RAM it
// occupies — from /proc/<pid>/status, or from ps on a system without /proc
// (macOS).
//
// VmRSS is the same figure ps reports as RSS; they are not different metrics.
// Deliberately NOT VmSize, which for a Go process includes over a gigabyte of
// reserved address space and would suggest Agency is enormous when it is not.
func RSSBytes(pid int) (int64, bool) {
	if !hasProcfs() {
		return rssBytesPS(pid)
	}
	b, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "status"))
	if err != nil {
		return 0, false
	}
	for line := range strings.SplitSeq(string(b), "\n") {
		if !strings.HasPrefix(line, "VmRSS:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0, false
		}
		kb, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return 0, false
		}
		return kb * 1024, true
	}
	return 0, false
}

// rssBytesPS is RSSBytes for a system without /proc: ps prints the resident
// set size in kilobytes.
func rssBytesPS(pid int) (int64, bool) {
	out, err := psOutput(pid, "rss")
	if err != nil {
		return 0, false
	}
	kb, err := strconv.ParseInt(out, 10, 64)
	if err != nil || kb <= 0 {
		return 0, false
	}
	return kb * 1024, true
}
