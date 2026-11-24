// Agency
// License: MIT

package tools

import "context"

type unattendedKey struct{}

// IsUnattended reports whether ctx belongs to a call run with no session
// behind it (ExecuteUnattended: a scheduled watch probe, a classifier run),
// whose result nothing would collect later.
func IsUnattended(ctx context.Context) bool {
	v, ok := ctx.Value(unattendedKey{}).(bool)
	return ok && v
}

func withUnattended(ctx context.Context) context.Context {
	return context.WithValue(ctx, unattendedKey{}, true)
}
