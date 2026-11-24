// Agency
// License: MIT

package tools

import (
	"go.bison9.com/agent/accounting"

	"go.bison9.com/agency/global"
)

// testIdentity is who the tests' model calls are billed to.
var testIdentity = accounting.Identity{Agent: "alice", Name: "Alice", Project: global.DefaultBilling}
