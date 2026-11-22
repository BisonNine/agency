// ClawEh
// License: MIT

// Package faulttest holds end-to-end fault-injection tests: the real agent
// loop and its model catalogue (fallback chains, health and cooldown) driven
// against in-process stub providers (internal/test/stubprovider) that fail on
// cue. It has no production code; see the _test.go files.
package faulttest
