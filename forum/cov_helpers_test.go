// ClawEh
// License: MIT
//
// Copyright (c) 2026 Tenebris Technologies Inc.

package forum

import (
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"go.bison9.com/agent/toolspec"
)

// Helpers in this file are prefixed cov so they cannot collide with other
// tests' helpers in the same package.

func TestJSONTypeName(t *testing.T) {
	cases := []struct {
		t    reflect.Type
		want string
	}{
		{reflect.TypeFor[**int](), "an integer"},
		{reflect.TypeFor[float64](), "a number"},
		{reflect.TypeFor[float32](), "a number"},
		{reflect.TypeFor[map[string]int](), "an object"},
		{reflect.TypeFor[[2]int](), "an array"},
		{reflect.TypeFor[bool](), "a boolean"},
		{reflect.TypeFor[complex128](), "complex128"},
	}
	for _, c := range cases {
		if got := jsonTypeName(c.t); got != c.want {
			t.Errorf("jsonTypeName(%v) = %q, want %q", c.t, got, c.want)
		}
	}
}

func TestDecodeIssueOtherError(t *testing.T) {
	got := decodeIssue(errors.New("boom"))
	if got.Path != "" || got.Message != "boom" {
		t.Errorf("decodeIssue = %+v, want message boom without path", got)
	}
}

func TestStructFieldsSkipsUnexportedAndDashed(t *testing.T) {
	type sample struct {
		Named    int `json:"named,omitempty"`
		Untagged string
		Skipped  bool `json:"-"`
		hidden   int
	}
	_ = sample{}.hidden
	got := structFields(reflect.TypeFor[sample]())
	keys := slices.Sorted(func(yield func(string) bool) {
		for k := range got {
			if !yield(k) {
				return
			}
		}
	})
	if want := []string{"Untagged", "named"}; !slices.Equal(keys, want) {
		t.Errorf("structFields keys = %v, want %v", keys, want)
	}
}

func TestJSONWalkerPositionOutOfRange(t *testing.T) {
	w := &jsonWalker{data: []byte("ab\ncd")}
	if got := w.position(-1); got != "" {
		t.Errorf("position(-1) = %q, want empty", got)
	}
	if got := w.position(99); got != "" {
		t.Errorf("position(99) = %q, want empty", got)
	}
	if got := w.position(4); got != " at line 2, column 2" {
		t.Errorf("position(4) = %q", got)
	}
}

func TestExplicitPresenceChecksIgnoreUndecodable(t *testing.T) {
	// The document has already decoded when these run; a document of the
	// wrong shape for the presence probe yields no issue rather than a panic.
	if got := explicitZeroLayerBudgets([]byte(`{"layers":"x"}`)); got != nil {
		t.Errorf("explicitZeroLayerBudgets = %v, want nil", got)
	}
	if got := explicitNullShares([]byte(`{"layers":"x"}`)); got != nil {
		t.Errorf("explicitNullShares = %v, want nil", got)
	}
}

func TestCanonicalConfigNumbers(t *testing.T) {
	got, err := canonicalConfig([]byte(`{"b":[1.0,1.5,2e3],"a":1e20,"c":12}`))
	if err != nil {
		t.Fatalf("canonicalConfig: %v", err)
	}
	if want := `{"a":1e+20,"b":[1,1.5,2000],"c":12}`; string(got) != want {
		t.Errorf("canonicalConfig = %s, want %s", got, want)
	}
	if _, err := canonicalConfig([]byte(`{"a":1}{"b":2}`)); err == nil || !strings.Contains(err.Error(), "trailing data") {
		t.Errorf("trailing data: err = %v", err)
	}
	if _, err := canonicalConfig([]byte(`{`)); err == nil {
		t.Error("malformed: want an error")
	}
	// A number beyond float64 is left as written.
	if got := canonicalNumbers(json.Number("1e999")); got != json.Number("1e999") {
		t.Errorf("canonicalNumbers(1e999) = %v", got)
	}
}

func TestConfigNameOfInvalidJSON(t *testing.T) {
	if got := configName([]byte("not json")); got != "" {
		t.Errorf("configName = %q, want empty", got)
	}
	if got := configName([]byte(`{"name":"Alice forum"}`)); got != "Alice forum" {
		t.Errorf("configName = %q", got)
	}
}

func TestSortedRunKeys(t *testing.T) {
	m := map[runKey]bool{
		{id: "b", run: 1}: true,
		{id: "a", run: 2}: true,
		{id: "a", run: 1}: true,
	}
	got := sortedRunKeys(m)
	want := []runKey{{"a", 1}, {"a", 2}, {"b", 1}}
	if !slices.Equal(got, want) {
		t.Errorf("sortedRunKeys = %v, want %v", got, want)
	}
}

func TestStatusTerminalUnknown(t *testing.T) {
	if Status("bogus").Terminal() {
		t.Error("an unknown status is not terminal")
	}
	if !StatusFailed.Terminal() || StatusPaused.Terminal() {
		t.Error("Terminal misclassifies a known status")
	}
}

func TestSnapshotLabel(t *testing.T) {
	if got := (&Snapshot{ForumID: "id-1"}).Label(); got != "id-1" {
		t.Errorf("Label without name = %q", got)
	}
	if got := (&Snapshot{ForumID: "id-1", Name: "Alice"}).Label(); got != "Alice" {
		t.Errorf("Label with name = %q", got)
	}
}

func TestOutputIDNonUUIDForum(t *testing.T) {
	a := outputID("not-a-uuid", "layer", "turn")
	if a != uuid.NewSHA1(uuid.NameSpaceOID, []byte("layer/turn")).String() {
		t.Errorf("outputID of a non-UUID forum = %s", a)
	}
	if b := outputID("not-a-uuid", "layer", "turn"); a != b {
		t.Error("outputID is not stable")
	}
	id := uuid.NewString()
	if outputID(id, "layer", "turn") == a {
		t.Error("outputID ignores the forum ID")
	}
}

func TestParticipantName(t *testing.T) {
	c := &forumController{
		parts: &Participants{Participants: map[string]ParticipantRecord{"alice": {Name: "Alice"}}},
		cfg:   &Config{Participants: map[string]Participant{"bob": {Name: "Bob"}, "carol": {}}},
	}
	for id, want := range map[string]string{"alice": "Alice", "bob": "Bob", "carol": "carol", "dave": "dave"} {
		if got := c.participantName(id); got != want {
			t.Errorf("participantName(%s) = %q, want %q", id, got, want)
		}
	}
}

func TestOutputContractJSONWithoutSchemaText(t *testing.T) {
	c := &forumController{cfg: &Config{}}
	got := c.outputContract(Output{Format: FormatJSON, Schema: "claim"}, false)
	if !strings.Contains(got, `JSON Schema "claim" given earlier`) {
		t.Errorf("outputContract = %q", got)
	}
}

func TestUnfence(t *testing.T) {
	cases := map[string]string{
		"```":                     "```",
		"```python\n{}\n```":      "```python\n{}\n```",
		"```json\n{\"a\":1}":      "```json\n{\"a\":1}",
		"```json\n{\"a\":1}\n```": `{"a":1}`,
		"  {\"a\":1}  ":           `{"a":1}`,
	}
	for in, want := range cases {
		if got := unfence(in); got != want {
			t.Errorf("unfence(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestValidateOutputUnknownFormat(t *testing.T) {
	_, issues := validateOutput(Output{Format: "yaml"}, "x", nil)
	if len(issues) != 1 || !strings.Contains(issues[0], `unknown output format "yaml"`) {
		t.Errorf("issues = %v", issues)
	}
}

func TestSchemaIssuesNonJSONInstance(t *testing.T) {
	s := mustCompileSchema(t, map[string]any{"type": "object"})
	issues := schemaIssues(s, []byte("{"))
	if len(issues) != 1 || !strings.HasPrefix(issues[0], "schema validation failed: ") {
		t.Errorf("issues = %v", issues)
	}
}

func TestRunArg(t *testing.T) {
	call := func(v any) *toolspec.ToolCall {
		args := map[string]any{}
		if v != nil {
			args["run"] = v
		}
		return &toolspec.ToolCall{Args: args}
	}
	good := map[string]struct {
		v    any
		want int
	}{
		"absent": {nil, 0},
		"float":  {float64(2), 2},
		"int":    {3, 3},
		"int64":  {int64(4), 4},
		"number": {json.Number("5"), 5},
	}
	for name, c := range good {
		got, err := runArg(call(c.v))
		if err != nil || got != c.want {
			t.Errorf("%s: runArg = %d, %v; want %d", name, got, err, c.want)
		}
	}
	for name, v := range map[string]any{
		"bad number": json.Number("x"),
		"string":     "1",
		"zero":       0,
		"fraction":   1.5,
		"huge":       float64(1 << 40),
	} {
		if _, err := runArg(call(v)); err == nil || !strings.Contains(err.Error(), "run argument must be a run number") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestJSONResultUnencodable(t *testing.T) {
	if _, err := jsonResult(make(chan int)); err == nil || !strings.Contains(err.Error(), "encode result") {
		t.Errorf("err = %v", err)
	}
}

func TestObjectArgUnencodable(t *testing.T) {
	call := &toolspec.ToolCall{Args: map[string]any{"config": map[string]any{"x": make(chan int)}}}
	_, err := objectArg(call, "config")
	if err == nil || !strings.Contains(err.Error(), "cannot be encoded as JSON") {
		t.Errorf("err = %v", err)
	}
}

func TestEncodeJSONValueUnencodable(t *testing.T) {
	if _, err := encodeJSONValue(make(chan int)); err == nil || !strings.HasPrefix(err.Error(), "encode JSON: ") {
		t.Errorf("err = %v", err)
	}
}

func TestProjectOutputMalformedDocument(t *testing.T) {
	_, err := projectOutput([]byte("{"), []string{"/a"})
	if err == nil || !strings.HasPrefix(err.Error(), "projection: ") {
		t.Errorf("err = %v", err)
	}
}

func TestCompileSchemaMalformed(t *testing.T) {
	if _, err := compileSchema(json.RawMessage("{")); err == nil || !strings.Contains(err.Error(), "compile schema") {
		t.Errorf("err = %v", err)
	}
}
