// Agency
// License: MIT
//
// Copyright (c) 2026 Tenebris Technologies Inc.

// Package mcpreg registers this host's MCP server in each CLI provider's own
// configuration, and reads whether it is registered there. It is what
// set-mcp.sh does, run by the gateway as its own service user.
//
// Everything Agency knows about each CLI is in one table (clis, keyed by the
// protocol of the SDK's modelspec catalogue): how to read the CLI's entry,
// how to register it, and the tool prefix it yields. Each CLI's own commands
// are used where it has them, since each CLI owns its configuration format;
// Cursor has none, so its JSON file is edited in place. Commands run without a
// shell, with an argument list, a timeout and the environment Agency gives
// CLI processes (internal/childenv).
package mcpreg

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"go.bison9.com/agent/modelspec"

	"go.bison9.com/agency/config"
	"go.bison9.com/agency/global"
	"go.bison9.com/agency/internal/childenv"
	"go.bison9.com/agency/mcpserver"
)

// Key is the name the host's MCP server is registered under in every CLI.
const Key = global.HostMCPServerKey

// DefaultTimeout bounds each command run against a CLI.
const DefaultTimeout = 30 * time.Second

// State is whether a CLI has the host's MCP server registered.
type State string

// States, in the words the WebUI and Check Up show for them.
const (
	// Registered: the CLI's entry points at this instance's endpoint.
	Registered State = "registered"
	// NotRegistered: the CLI has no entry under Key.
	NotRegistered State = "not_registered"
	// OtherAddress: the CLI's entry points somewhere else (Status.Address).
	OtherAddress State = "other_address"
	// NotInstalled: the CLI's binary was not found.
	NotInstalled State = "not_installed"
	// NotChecked: the CLI's configuration could not be read.
	NotChecked State = "not_checked"
	// HostOff: the MCP host is off, so there is no endpoint to register.
	HostOff State = "host_off"
)

// Status is one CLI's registration state.
type Status struct {
	Protocol string `json:"protocol"`
	Label    string `json:"label"`
	State    State  `json:"state"`
	// Address is what the CLI's entry points at, for OtherAddress.
	Address string `json:"address,omitempty"`
}

// Errors Register returns besides a failed command.
var (
	// ErrUnknownCLI: the protocol is not a CLI in the catalogue.
	ErrUnknownCLI = errors.New("unknown CLI")
	// ErrNotInstalled: the CLI's binary was not found.
	ErrNotInstalled = errors.New("CLI not installed")
	// ErrHostOff: no endpoint to register (the MCP host is off).
	ErrHostOff = errors.New("tool server is off")
	// ErrReplaceRequired: the CLI's entry points elsewhere and the caller did
	// not ask to replace it. The Status returned with it names that address.
	ErrReplaceRequired = errors.New("registered to a different address")
)

// InternalURL is this instance's internal MCP endpoint, the one each CLI is
// registered with: the mcp_host listen address and the /internal path. It is
// "" when the MCP host is off.
func InternalURL(cfg *config.Config) string {
	if cfg == nil || !cfg.MCPHostEffectivelyEnabled() {
		return ""
	}
	listen := strings.TrimSpace(cfg.MCPHost.Listen)
	if listen == "" {
		listen = mcpserver.DefaultListen
	}
	return "http://" + listen + mcpserver.InternalEndpointPath
}

// ToolPrefix is the prefix a model on protocol sees the host's MCP tools
// under: OpenCode's own naming for OpenCode, mcp__agency__ for every other
// protocol.
func ToolPrefix(protocol string) string {
	if c, ok := clis[protocol]; ok {
		return c.toolPrefix
	}
	return global.HostMCPToolPrefix
}

// RunFunc runs one program without a shell: path with args, in dir, with
// env. It returns the program's standard output and standard error.
type RunFunc func(ctx context.Context, dir string, env []string, path string, args ...string) (stdout, stderr []byte, err error)

// Registrar reads and writes the host's entry in each CLI. Its zero value is
// not usable; build one with New. Safe for concurrent use: calls for one CLI
// are serialised, calls for different CLIs run in parallel.
type Registrar struct {
	home     string
	env      []string
	run      RunFunc
	lookPath func(string) (string, error)
	timeout  time.Duration

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

// Option configures a Registrar.
type Option func(*Registrar)

// WithHome sets the home directory whose CLI configuration is read and
// written (default: the process's).
func WithHome(home string) Option { return func(r *Registrar) { r.home = home } }

// WithEnv sets the environment the CLIs run with (default childenv.CLI()).
func WithEnv(env []string) Option { return func(r *Registrar) { r.env = env } }

// WithRunner replaces how commands are run (default: os/exec).
func WithRunner(run RunFunc) Option { return func(r *Registrar) { r.run = run } }

// WithLookPath replaces how a CLI's binary is found (default exec.LookPath,
// the check the CLI providers list uses).
func WithLookPath(fn func(string) (string, error)) Option {
	return func(r *Registrar) { r.lookPath = fn }
}

// WithTimeout bounds each command (default DefaultTimeout).
func WithTimeout(d time.Duration) Option { return func(r *Registrar) { r.timeout = d } }

// New returns a Registrar for the service user's CLIs.
func New(opts ...Option) *Registrar {
	r := &Registrar{
		run:      execRun,
		lookPath: exec.LookPath,
		timeout:  DefaultTimeout,
		locks:    map[string]*sync.Mutex{},
	}
	for _, o := range opts {
		o(r)
	}
	if r.home == "" {
		if home, err := os.UserHomeDir(); err == nil {
			r.home = home
		}
	}
	if r.env == nil {
		r.env = childenv.CLI()
	}
	return r
}

// Check reads one CLI's registration against url (InternalURL; "" when the
// MCP host is off).
func (r *Registrar) Check(ctx context.Context, protocol, url string) (Status, error) {
	agent, c, err := lookup(protocol)
	if err != nil {
		return Status{}, err
	}
	unlock := r.lock(agent.Protocol)
	defer unlock()
	st, _ := r.check(ctx, agent, c, url)
	return st, nil
}

// CheckAll reads every CLI of the catalogue in parallel, in catalogue order.
func (r *Registrar) CheckAll(ctx context.Context, url string) []Status {
	out := make([]Status, len(modelspec.CLIAgents))
	var wg sync.WaitGroup
	for i := range modelspec.CLIAgents {
		wg.Go(func() {
			st, err := r.Check(ctx, modelspec.CLIAgents[i].Protocol, url)
			if err != nil { // a catalogue CLI missing from the table
				st = Status{Protocol: modelspec.CLIAgents[i].Protocol, Label: modelspec.CLIAgents[i].Label, State: NotChecked}
			}
			out[i] = st
		})
	}
	wg.Wait()
	return out
}

// Register adds or updates the host's entry in one CLI so it points at url,
// then reads it back. The current entry is read first, under the CLI's lock:
// one that points at another address is replaced only with replace set
// (ErrReplaceRequired otherwise, with that address in the Status).
func (r *Registrar) Register(ctx context.Context, protocol, url string, replace bool) (Status, error) {
	agent, c, err := lookup(protocol)
	if err != nil {
		return Status{}, err
	}
	unlock := r.lock(agent.Protocol)
	defer unlock()

	st, bin := r.check(ctx, agent, c, url)
	switch {
	case st.State == NotInstalled:
		return st, ErrNotInstalled
	case st.State == HostOff:
		return st, ErrHostOff
	case st.State == OtherAddress && !replace:
		return st, ErrReplaceRequired
	case r.home == "":
		return st, errors.New("no home directory")
	}
	existing := st.State == Registered || st.State == OtherAddress
	if err := c.register(ctx, r, bin, url, existing); err != nil {
		return st, err
	}
	after, _ := r.check(ctx, agent, c, url)
	return after, nil
}

// check is Check under the caller's lock; it also returns the binary found.
func (r *Registrar) check(ctx context.Context, agent *modelspec.CLIAgent, c cli, url string) (Status, string) {
	st := Status{Protocol: agent.Protocol, Label: agent.Label}
	bin, err := r.lookPath(agent.Binary)
	if err != nil {
		st.State = NotInstalled
		return st, ""
	}
	if url == "" {
		st.State = HostOff
		return st, bin
	}
	if r.home == "" {
		st.State = NotChecked
		return st, bin
	}
	e, err := c.read(ctx, r, bin)
	switch {
	case err != nil:
		st.State = NotChecked
	case !e.found:
		st.State = NotRegistered
	case sameURL(e.address, url):
		st.State = Registered
	default:
		st.State = OtherAddress
		st.Address = e.address
	}
	return st, bin
}

// lock serialises the calls for one CLI.
func (r *Registrar) lock(protocol string) func() {
	r.mu.Lock()
	m, ok := r.locks[protocol]
	if !ok {
		m = &sync.Mutex{}
		r.locks[protocol] = m
	}
	r.mu.Unlock()
	m.Lock()
	return m.Unlock
}

// exec runs bin with args in the home directory under the timeout.
func (r *Registrar) exec(ctx context.Context, bin string, args ...string) (stdout, stderr []byte, err error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	stdout, stderr, err = r.run(ctx, r.home, r.env, bin, args...)
	if err != nil && ctx.Err() != nil {
		err = fmt.Errorf("%s %s: timed out: %w", bin, strings.Join(args, " "), ctx.Err())
	}
	return stdout, stderr, err
}

// lookup finds a protocol's catalogue entry and its row in the table.
func lookup(protocol string) (*modelspec.CLIAgent, cli, error) {
	agent := modelspec.CLIAgentByProtocol(protocol)
	if agent == nil {
		return nil, cli{}, fmt.Errorf("%w: %q", ErrUnknownCLI, protocol)
	}
	c, ok := clis[agent.Protocol]
	if !ok {
		return nil, cli{}, fmt.Errorf("%w: %q", ErrUnknownCLI, protocol)
	}
	return agent, c, nil
}

// sameURL compares two endpoint URLs, ignoring a trailing slash.
func sameURL(a, b string) bool {
	return strings.TrimRight(strings.TrimSpace(a), "/") == strings.TrimRight(strings.TrimSpace(b), "/")
}
