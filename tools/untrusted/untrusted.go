// Package untrusted re-exports the SDK's toolkit/untrusted under the import
// path the host's tools have always used. Wrap marks tool output that came
// from outside the process (web pages, search results, upstream MCP servers)
// so the model can tell retrieved data from instructions.
package untrusted

import "go.bison9.com/agent/toolkit/untrusted"

// Preamble is the single line that precedes every wrapped block.
const Preamble = untrusted.Preamble

// Placeholder replaces every neutralised token inside the content.
const Placeholder = untrusted.Placeholder

var (
	// Wrap returns content marked as untrusted external data for the model.
	Wrap = untrusted.Wrap
	// Unwrap returns the content of a block Wrap made.
	Unwrap = untrusted.Unwrap
	// Neutralise replaces chat-template control tokens in content.
	Neutralise = untrusted.Neutralise
)
