// Agency
// License: MIT

package api

import (
	"encoding/json"
	"reflect"
	"testing"
)

// restoreMaskedSecrets replaces masked credentials with the stored ones and
// leaves every other value as sent. Each case is the request body, the stored
// config, and the body that must come out.
func TestRestoreMaskedSecrets(t *testing.T) {
	const (
		keyA = "sk-aaaaaaaaaaaa1111"
		keyB = "sk-bbbbbbbbbbbb2222"
		keyC = "sk-cccccccccccc3333"
	)
	mask := maskAPIKey
	for _, tc := range []struct {
		name, body, stored, want string
	}{
		{
			name:   "masked key restored, new key kept",
			body:   `{"a":{"api_key":"` + mask(keyA) + `","token":"new-token"}}`,
			stored: `{"a":{"api_key":"` + keyA + `","token":"old-token"}}`,
			want:   `{"a":{"api_key":"` + keyA + `","token":"new-token"}}`,
		},
		{
			name:   "masked key with nothing stored stays masked",
			body:   `{"api_key":"****"}`,
			stored: `{}`,
			want:   `{"api_key":"****"}`,
		},
		{
			name:   "object where the store has a scalar is left alone",
			body:   `{"a":{"api_key":"****"}}`,
			stored: `{"a":"x"}`,
			want:   `{"a":{"api_key":"****"}}`,
		},
		{
			name:   "list where the store has an object is left alone",
			body:   `{"a":[{"api_key":"****"}]}`,
			stored: `{"a":{"api_key":"` + keyA + `"}}`,
			want:   `{"a":[{"api_key":"****"}]}`,
		},
		{
			name:   "key list: masks follow their keys across a reorder",
			body:   `{"api_keys":["` + mask(keyB) + `","` + mask(keyA) + `","sk-new"]}`,
			stored: `{"api_keys":["` + keyA + `","` + keyB + `"]}`,
			want:   `{"api_keys":["` + keyB + `","` + keyA + `","sk-new"]}`,
		},
		{
			name:   "key list: a mask matching no stored key takes the same position",
			body:   `{"api_keys":["****","****"]}`,
			stored: `{"api_keys":["` + keyA + `","` + keyB + `","` + keyC + `"]}`,
			want:   `{"api_keys":["` + keyA + `","` + keyB + `"]}`,
		},
		{
			name:   "key list: a mask beyond the stored list stays masked",
			body:   `{"api_keys":["****"]}`,
			stored: `{"api_keys":[]}`,
			want:   `{"api_keys":["****"]}`,
		},
		{
			name:   "key list where the store has none",
			body:   `{"api_keys":["****"]}`,
			stored: `{"api_keys":"x"}`,
			want:   `{"api_keys":["****"]}`,
		},
		{
			name:   "env map: masked values restored by key, others kept",
			body:   `{"env":{"A":"****","B":"plain","C":7,"D":"****"}}`,
			stored: `{"env":{"A":"secret-a","B":"old","D":5}}`,
			want:   `{"env":{"A":"secret-a","B":"plain","C":7,"D":"****"}}`,
		},
		{
			name:   "elements without identity match by position",
			body:   `{"list":[{"api_key":"****"},{"api_key":"****"}]}`,
			stored: `{"list":[{"api_key":"` + keyA + `"}]}`,
			want:   `{"list":[{"api_key":"` + keyA + `"},{"api_key":"****"}]}`,
		},
		{
			name:   "scalar elements match by position",
			body:   `{"list":[["****"],"x","y"]}`,
			stored: `{"list":[["z"]]}`,
			want:   `{"list":[["****"],"x","y"]}`,
		},
		{
			name:   "named element matches by name past other values",
			body:   `{"providers":[{"name":"b","api_key":"****"}]}`,
			stored: `{"providers":["junk",{"name":"a","api_key":"` + keyA + `"},{"name":"b","api_key":"` + keyB + `"}]}`,
			want:   `{"providers":[{"name":"b","api_key":"` + keyB + `"}]}`,
		},
		{
			name:   "new named element gets no other element's key",
			body:   `{"providers":[{"name":"c","api_key":"****"}]}`,
			stored: `{"providers":[{"name":"a","api_key":"` + keyA + `"}]}`,
			want:   `{"providers":[{"name":"c","api_key":"****"}]}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := restoreMaskedSecrets([]byte(tc.body), []byte(tc.stored))
			if err != nil {
				t.Fatal(err)
			}
			if !jsonEqual(t, got, []byte(tc.want)) {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
		})
	}
}

func TestRestoreMaskedSecrets_BadJSON(t *testing.T) {
	if _, err := restoreMaskedSecrets([]byte("{"), []byte("{}")); err == nil {
		t.Fatal("bad body must fail")
	}
	if _, err := restoreMaskedSecrets([]byte("{}"), []byte("{")); err == nil {
		t.Fatal("bad stored config must fail")
	}
}

func jsonEqual(t *testing.T, a, b []byte) bool {
	t.Helper()
	var x, y any
	if err := json.Unmarshal(a, &x); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &y); err != nil {
		t.Fatal(err)
	}
	return reflect.DeepEqual(x, y)
}
