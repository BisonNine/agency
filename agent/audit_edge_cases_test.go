// Agency
// License: MIT

package agent

import (
	"encoding/json"
	"math"
	"testing"

	"go.bison9.com/agent/spawnllm"
)

// Arguments that cannot be encoded leave the details with the chat and a
// placeholder for the arguments.
func TestAudit_UnencodableArguments(t *testing.T) {
	got := toolCallAuditDetails(`chat "42"`, "", spawnllm.ToolCall{Name: "file_read_bytes", Arguments: map[string]any{"path": "a.txt", "offset": math.Inf(1)}})
	var details map[string]string
	if err := json.Unmarshal([]byte(got), &details); err != nil {
		t.Fatalf("details are not JSON: %q: %v", got, err)
	}
	if details["chat_id"] != `chat "42"` || details["args"] != "<unmarshalable>" {
		t.Fatalf("details = %v", details)
	}
}
