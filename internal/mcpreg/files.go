// Agency
// License: MIT
//
// Copyright (c) 2026 Tenebris Technologies Inc.

package mcpreg

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"go.bison9.com/agency/fileutil"
)

// maxOutput caps what is kept of a command's output.
const maxOutput = 1 << 20

// newConfigDirMode and newConfigFileMode are the modes of a CLI configuration
// folder and file Agency creates: the service user's only.
const (
	newConfigDirMode  os.FileMode = 0o700
	newConfigFileMode os.FileMode = 0o600
)

// execRun is the default RunFunc: os/exec, no shell, no standard input.
func execRun(ctx context.Context, dir string, env []string, path string, args ...string) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, path, args...) //nolint:gosec // a fixed CLI binary with fixed arguments (see clis)
	cmd.Dir = dir
	cmd.Env = env
	// A CLI may leave a helper holding its output open; stop waiting for it
	// shortly after the command itself was ended.
	cmd.WaitDelay = 2 * time.Second
	var stdout, stderr limitedBuffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

// limitedBuffer keeps the first maxOutput bytes written to it.
type limitedBuffer struct{ bytes.Buffer }

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := maxOutput - b.Len(); room > 0 {
		if len(p) > room {
			b.Buffer.Write(p[:room])
		} else {
			b.Buffer.Write(p)
		}
	}
	return len(p), nil
}

// errNotObject is a file, or a member on the edited path, that is not a JSON
// object.
var errNotObject = errors.New("not a JSON object")

// editJSONFile sets the member at path in the JSON file to value, keeping
// every other member, and the order of members, as they are. Objects missing
// along the path are created. A file that does not exist is created only
// with create set (its folder too, for the service user only); a file that
// does not parse is left untouched and is an error. A link is followed, so
// the file it points at is the one written; its mode is kept.
func editJSONFile(path string, keys []string, value json.RawMessage, create bool) error {
	target := path
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		target = resolved
	}
	mode := newConfigFileMode
	data, err := os.ReadFile(target) //nolint:gosec // a fixed CLI config path under the service user's home
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if !create {
			return fmt.Errorf("%s: %w", path, err)
		}
		if merr := fileutil.MkdirAllExact(filepath.Dir(target), newConfigDirMode); merr != nil {
			return merr
		}
		data = []byte("{}")
	case err != nil:
		return err
	default:
		if fi, serr := os.Stat(target); serr == nil {
			mode = fi.Mode().Perm()
		}
	}
	out, err := setJSONMember(data, keys, value)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, out, "", "  "); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	pretty.WriteByte('\n')
	return fileutil.WriteFileAtomic(target, pretty.Bytes(), mode)
}

// member is one name and value of a JSON object, in document order.
type member struct {
	name  string
	value json.RawMessage
}

// setJSONMember returns the JSON object doc with the member at keys set to
// value, every other member kept in its place.
func setJSONMember(doc []byte, keys []string, value json.RawMessage) ([]byte, error) {
	if len(keys) == 0 {
		return nil, errors.New("no member named")
	}
	members, err := objectMembers(doc)
	if err != nil {
		return nil, err
	}
	name := keys[0]
	newValue := value
	found := false
	for i := range members {
		if members[i].name != name {
			continue
		}
		found = true
		if len(keys) > 1 {
			if newValue, err = setJSONMember(members[i].value, keys[1:], value); err != nil {
				return nil, fmt.Errorf("%s: %w", name, err)
			}
		}
		members[i].value = newValue
	}
	if !found {
		if len(keys) > 1 {
			if newValue, err = setJSONMember([]byte("{}"), keys[1:], value); err != nil {
				return nil, err
			}
		}
		members = append(members, member{name: name, value: newValue})
	}
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, m := range members {
		if i > 0 {
			buf.WriteByte(',')
		}
		n, err := json.Marshal(m.name)
		if err != nil {
			return nil, err
		}
		buf.Write(n)
		buf.WriteByte(':')
		buf.Write(m.value)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// objectMembers splits a JSON object into its members, in order.
func objectMembers(doc []byte) ([]member, error) {
	dec := json.NewDecoder(bytes.NewReader(doc))
	tok, err := dec.Token()
	if err != nil {
		return nil, errUnreadable
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, errNotObject
	}
	var members []member
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, errUnreadable
		}
		name, ok := tok.(string)
		if !ok {
			return nil, errUnreadable
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, errUnreadable
		}
		members = append(members, member{name: name, value: value})
	}
	if _, err := dec.Token(); err != nil {
		return nil, errUnreadable
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errUnreadable
	}
	return members, nil
}
