// ClawEh
// License: MIT
//
// Copyright (c) 2026 Tenebris Technologies Inc.

package classifier

import (
	"context"
	"encoding/json"
	"math"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestSchemaTypes(t *testing.T) {
	cases := []struct {
		name string
		prop any
		want []string
	}{
		{"not a schema", "string", nil},
		{"one type", map[string]any{"type": "string"}, []string{"string"}},
		{"string list", map[string]any{"type": []string{"integer", "null"}}, []string{"integer", "null"}},
		{"any list skips non-text", map[string]any{"type": []any{"number", 7, "null"}}, []string{"number", "null"}},
		{"no type", map[string]any{"enum": []any{"a"}}, nil},
		{"type of another kind", map[string]any{"type": 3}, nil},
	}
	for _, tc := range cases {
		if got := schemaTypes(tc.prop); !slices.Equal(got, tc.want) {
			t.Errorf("%s: schemaTypes = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestValueKind(t *testing.T) {
	cases := []struct {
		v    any
		want int
	}{
		{"x", kindText},
		{true, kindBool},
		{nil, kindNull},
		{[]any{1}, kindList},
		{map[string]any{}, kindObject},
		{2.0, kindWhole},
		{2.5, kindFraction},
		{3, kindWhole},
		{int64(4), kindWhole},
		{json.Number("12"), kindWhole},
		{json.Number("1.5"), kindFraction},
		{json.Number("nope"), kindFraction},
		{struct{}{}, kindOther},
	}
	for _, tc := range cases {
		if got := valueKind(tc.v); got != tc.want {
			t.Errorf("valueKind(%#v) = %d, want %d", tc.v, got, tc.want)
		}
	}
}

func TestTypeAccepts(t *testing.T) {
	cases := []struct {
		types []string
		kind  int
		want  bool
	}{
		{[]string{"string"}, kindText, true},
		{[]string{"string"}, kindWhole, false},
		{[]string{"integer"}, kindWhole, true},
		{[]string{"integer"}, kindFraction, false},
		{[]string{"number"}, kindWhole, true},
		{[]string{"number"}, kindFraction, true},
		{[]string{"number"}, kindText, false},
		{[]string{"boolean"}, kindBool, true},
		{[]string{"null"}, kindNull, true},
		{[]string{"null"}, kindText, false},
		{[]string{"array"}, kindList, true},
		{[]string{"object"}, kindObject, true},
		{[]string{"integer", "null"}, kindNull, true},
		{[]string{"uuid"}, kindText, true}, // unknown type: not refused
	}
	for _, tc := range cases {
		if got := typeAccepts(tc.types, tc.kind); got != tc.want {
			t.Errorf("typeAccepts(%v, %s) = %v, want %v", tc.types, kindWords[tc.kind], got, tc.want)
		}
	}
	if got := typeWords([]string{"integer", "uuid"}); got != "a whole number or uuid" {
		t.Errorf("typeWords = %q", got)
	}
}

// An enum given as a Go []string (a schema built in code, not decoded
// JSON) is checked like a decoded one.
func TestCheckValueStringEnum(t *testing.T) {
	schema := map[string]any{"enum": []string{"asc", "desc"}}
	if got := checkValue("args.order", "asc", schema); len(got) != 0 {
		t.Errorf("checkValue(asc) = %v, want none", got)
	}
	got := checkValue("args.order", "up", schema)
	if len(got) != 1 || got[0] != `args.order must be one of "asc", "desc"; it is "up".` {
		t.Errorf("checkValue(up) = %v", got)
	}
	got = checkValue("args.order", 2.0, schema)
	if len(got) != 1 || !strings.Contains(got[0], "it is 2.") {
		t.Errorf("checkValue(2) = %v", got)
	}
}

// A value JSON cannot encode is named in Go form rather than lost.
func TestCompactJSONUnencodable(t *testing.T) {
	if got := compactJSON(math.Inf(1)); got != "+Inf" {
		t.Errorf("compactJSON(+Inf) = %q, want +Inf", got)
	}
}

func TestOffsetValue(t *testing.T) {
	cases := []struct {
		v    any
		want int
		ok   bool
	}{
		{10.0, 10, true},
		{7, 7, true},
		{json.Number("25"), 25, true},
		{json.Number("x"), 0, false},
		{-1.0, 0, false},
		{1.5, 0, false},
		{"10", 0, false},
	}
	for _, tc := range cases {
		got, ok := offsetValue(tc.v)
		if got != tc.want || ok != tc.ok {
			t.Errorf("offsetValue(%#v) = %d, %v; want %d, %v", tc.v, got, ok, tc.want, tc.ok)
		}
	}
}

func TestCursorValueOtherKinds(t *testing.T) {
	if _, _, ok := cursorValue(true); ok {
		t.Error("cursorValue(true) ok, want no cursor")
	}
	if _, _, ok := cursorValue("  "); ok {
		t.Error("cursorValue(blank) ok, want no cursor")
	}
}

func TestTruncatedErrorText(t *testing.T) {
	cases := []struct {
		e    truncatedError
		want string
	}{
		{
			truncatedError{tool: "mail_list", pages: 1},
			"source mail_list lists more items than the 1 page one run reads, so the rest were not seen; make source.args list fewer items or raise source.paging.max_pages",
		},
		{
			truncatedError{tool: "mail_list", pages: 3, ceiling: 10},
			"source mail_list lists more items than the 3 pages one run reads, so the rest were not seen; make source.args list fewer items or raise source.paging.max_pages (this install allows 10)",
		},
		{
			truncatedError{tool: "mail_list", pages: 10, ceiling: 10},
			"source mail_list lists more items than the 10 pages one run reads, so the rest were not seen; make source.args list fewer items",
		},
	}
	for _, tc := range cases {
		if got := tc.e.Error(); got != tc.want {
			t.Errorf("Error() = %q\nwant %q", got, tc.want)
		}
	}
	if got := sourceName(Source{Tool: "  "}); got != "the source tool" {
		t.Errorf("sourceName(blank) = %q", got)
	}
}

func TestAllowedText(t *testing.T) {
	if got := allowedText(nil); got != "no placeholder is allowed there" {
		t.Errorf("allowedText(nil) = %q", got)
	}
	got := allowedText([]string{placeholderItem, placeholderFetched, placeholderCategory})
	if got != "use {{item.<path>}}, {{fetched.<name>}}, {{category}}" {
		t.Errorf("allowedText(all) = %q", got)
	}
}

func TestPlainJSONNumbers(t *testing.T) {
	if got := plainJSON(json.Number("1.25")); got != 1.25 {
		t.Errorf("plainJSON(1.25) = %#v", got)
	}
	if got := plainJSON(json.Number("1e999")); got != "1e999" {
		t.Errorf("plainJSON(1e999) = %#v, want its text", got)
	}
	if got := plainJSON(json.Number("123456789012345678901")); got != "123456789012345678901" {
		t.Errorf("plainJSON(huge int) = %#v, want its text", got)
	}
}

// substitute fails an item on a malformed placeholder a hand-edited file can
// still hold, and passes nil arguments through.
func TestSubstituteEdges(t *testing.T) {
	if got, err := substitute(nil, nil, nil, ""); got != nil || err != nil {
		t.Errorf("substitute(nil) = %v, %v", got, err)
	}
	if _, err := substitute(map[string]any{"a": "{{bogus}}"}, nil, nil, ""); err == nil || !strings.Contains(err.Error(), "unknown placeholder") {
		t.Errorf("substitute(bogus) = %v, want unknown placeholder", err)
	}
	if _, err := substitute(map[string]any{"a": "{{category}}"}, nil, nil, ""); err == nil {
		t.Error("substitute({{category}} with none) = nil, want unresolved")
	}
}

func TestValidPathAndItemID(t *testing.T) {
	for p, want := range map[string]bool{"a.b": true, "": false, "a b": false, "a..b": false, "a.": false} {
		if got := validPath(p); got != want {
			t.Errorf("validPath(%q) = %v, want %v", p, got, want)
		}
	}
	if _, ok := itemID(" "); ok {
		t.Error("itemID(blank) ok")
	}
	if got, ok := itemID(json.Number("42")); !ok || got != "42" {
		t.Errorf("itemID(42) = %q, %v", got, ok)
	}
	if _, ok := itemID(true); ok {
		t.Error("itemID(true) ok")
	}
	if _, err := decodeJSON(`{} []`); err == nil || err.Error() != "trailing data" {
		t.Errorf("decodeJSON(trailing) = %v", err)
	}
	if got := plainArgs(nil); got == nil || len(got) != 0 {
		t.Errorf("plainArgs(nil) = %#v, want an empty map", got)
	}
	src := map[string]any{"n": json.Number("3"), "nested": map[string]any{"f": json.Number("1.5")}}
	got := plainArgs(src)
	nested, isMap := got["nested"].(map[string]any)
	if got["n"] != float64(3) || !isMap || nested["f"] != 1.5 {
		t.Errorf("plainArgs = %#v, want plain numbers", got)
	}
	got["n"] = "changed"
	if src["n"] != json.Number("3") {
		t.Error("plainArgs returned the definition's own map")
	}
}

func TestClassifierToolRefusal(t *testing.T) {
	if got := ClassifierToolRefusal("classifier_list", ""); got != "classifier_list can't be used by a classifier." {
		t.Errorf("no field: %q", got)
	}
	if got := ClassifierToolRefusal("classifier_list", "source"); got != "classifier_list can't be used by a classifier (source)." {
		t.Errorf("field: %q", got)
	}
}

func TestHostDefaults(t *testing.T) {
	h := &Host{}
	c := h.clock()
	before := time.Now()
	if now := c.Now(); now.Before(before.Add(-time.Second)) {
		t.Errorf("system clock Now = %v", now)
	}
	select {
	case <-c.After(0):
	case <-time.After(10 * time.Second):
		t.Error("system clock After(0) did not fire")
	}
	if got := h.ceilings(); got != (Ceilings{}) {
		t.Errorf("ceilings() with none = %+v", got)
	}
	if got := h.wrapUntrusted("x"); got != "x" {
		t.Errorf("wrapUntrusted with none = %q", got)
	}
	if got := ClassifierID(context.Background()); got != "" {
		t.Errorf("ClassifierID(none) = %q", got)
	}
	if got := ClassifierID(withClassifier(context.Background(), "spam")); got != "spam" {
		t.Errorf("ClassifierID = %q", got)
	}
	if !(&ValidationError{}).Refusal() {
		t.Error("ValidationError is not a refusal")
	}
	if got := (Ceilings{}).processedDays(); got != DefaultProcessedDays {
		t.Errorf("processedDays default = %d", got)
	}
	if got := (&modelError{}).summary(); got != "the classifier model: the call failed" {
		t.Errorf("modelError summary = %q", got)
	}
	if got := roleRank("other"); got <= roleRank(roleAction) {
		t.Errorf("roleRank(other) = %d, want after action", got)
	}
}

func TestJSONListRoundTrip(t *testing.T) {
	if jsonList(nil) != nil {
		t.Error("jsonList(nil) not NULL")
	}
	if got := fromJSONList(`["a","b"]`); !slices.Equal(got, []string{"a", "b"}) {
		t.Errorf("fromJSONList = %v", got)
	}
	if got := fromJSONList(`{`); got != nil {
		t.Errorf("fromJSONList(bad) = %v", got)
	}
}

func TestSmallTextHelpers(t *testing.T) {
	if got := withPeriod("done."); got != "done." {
		t.Errorf("withPeriod(done.) = %q", got)
	}
	if got := withPeriod("done"); got != "done." {
		t.Errorf("withPeriod(done) = %q", got)
	}
	if slicesAnyText([]NamedText{{Name: "a", Text: " "}}) {
		t.Error("slicesAnyText(blank) = true")
	}
	if !slicesAnyText([]NamedText{{Name: "a", Text: " "}, {Name: "b", Text: "x"}}) {
		t.Error("slicesAnyText(text) = false")
	}
	if got := nonNilArgs(nil); got == nil || len(got) != 0 {
		t.Errorf("nonNilArgs(nil) = %#v", got)
	}
	if got := argError("bad folder").Error(); got != "bad folder" {
		t.Errorf("argError = %q", got)
	}
	if _, err := jsonResult(make(chan int)); err == nil {
		t.Error("jsonResult(chan) = nil error")
	}
}

// previewAction leaves a decision whose category has no action as it is.
func TestPreviewActionWithoutAction(t *testing.T) {
	def := &Definition{Actions: map[string]Action{}}
	d := &decision{}
	d.Category = "Spam"
	previewAction(def, listedItem{id: "m1"}, d)
	if d.Action != "" || d.ActionArgs != "" || d.Error != "" {
		t.Errorf("previewAction(no action) changed the decision: %+v", d.Decision)
	}
}
