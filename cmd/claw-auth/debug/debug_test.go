package debug

import (
	"bytes"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// captureLog turns Debug on and collects what the log package writes.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prevOut, prevFlags, prevDebug := log.Writer(), log.Flags(), Debug
	log.SetOutput(&buf)
	log.SetFlags(0)
	Debug = true
	t.Cleanup(func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
		Debug = prevDebug
	})
	return &buf
}

func TestMaskSensitiveData(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{
			`{"access_token":"at-1","refresh_token":"rt-1","name":"Alice"}`,
			`{"access_token":"***","refresh_token":"***","name":"Alice"}`,
		},
		{
			"client_id=abc&client_secret=s3cret&code=c0de&code_verifier=v&grant_type=authorization_code",
			"client_id=abc&client_secret=***&code=***&code_verifier=v&grant_type=authorization_code",
		},
		{"code=c0de", "code=***"},
		{"password=hunter2\nother=1", "password=***\nother=1"},
		{`{"code":""}`, `{"code":""}`},
		{"plain text", "plain text"},
	} {
		if got := maskSensitiveData(tc.in); got != tc.want {
			t.Errorf("maskSensitiveData(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestMaskAuthHeader(t *testing.T) {
	for in, want := range map[string]string{
		"Bearer abcdefgh": "Bearer abcd***",
		"Bearer ab":       "Bearer ab***",
		"opaque":          "***",
		"a b c":           "***",
	} {
		if got := maskAuthHeader(in); got != want {
			t.Errorf("maskAuthHeader(%q) = %q, want %q", in, got, want)
		}
	}
}

// With Debug off nothing is logged and the bodies are left alone.
func TestLogHTTP_Off(t *testing.T) {
	buf := captureLog(t)
	Debug = false
	req := httptest.NewRequest(http.MethodPost, "http://x/token", strings.NewReader("code=1"))
	LogHTTPRequest(req)
	LogHTTPResponse(&http.Response{Status: "200 OK", StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("ok"))})
	if buf.Len() != 0 {
		t.Fatalf("logged with Debug off: %q", buf.String())
	}
}

// A logged request masks the Authorization header and the secrets in the
// body, and the body can still be read afterwards.
func TestLogHTTPRequest(t *testing.T) {
	buf := captureLog(t)
	req := httptest.NewRequest(http.MethodPost, "http://fusion.example/api/v1/oauth/tokens",
		strings.NewReader("client_id=abc&client_secret=s3cret"))
	req.Header.Set("Authorization", "Bearer tok-123456")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	LogHTTPRequest(req)
	out := buf.String()
	for _, want := range []string{"Method: POST", "URL: http://fusion.example/api/v1/oauth/tokens", "Authorization: Bearer tok-***", "Body: client_id=abc&client_secret=***"} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "s3cret") || strings.Contains(out, "tok-123456") {
		t.Fatalf("log leaks a secret:\n%s", out)
	}
	body, err := io.ReadAll(req.Body)
	if err != nil || string(body) != "client_id=abc&client_secret=s3cret" {
		t.Fatalf("body after logging = %q %v", body, err)
	}
}

// A logged response masks tokens in the body and leaves it readable.
func TestLogHTTPResponse(t *testing.T) {
	buf := captureLog(t)
	resp := &http.Response{
		Status: "200 OK", StatusCode: http.StatusOK,
		Header: http.Header{"X-Trace": {"t1"}},
		Body:   io.NopCloser(strings.NewReader(`{"access_token":"at-9"}`)),
	}
	LogHTTPResponse(resp)
	out := buf.String()
	for _, want := range []string{"Status: 200 OK", "Status Code: 200", "X-Trace: t1", `Body: {"access_token":"***"}`} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q:\n%s", want, out)
		}
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil || string(body) != `{"access_token":"at-9"}` {
		t.Fatalf("body after logging = %q", body)
	}
}

type failReader struct{}

func (failReader) Read([]byte) (int, error) { return 0, errors.New("broken pipe") }
func (failReader) Close() error             { return errors.New("close failed") }

// An unreadable body is reported, not fatal.
func TestLogHTTP_UnreadableBody(t *testing.T) {
	buf := captureLog(t)
	req := httptest.NewRequest(http.MethodPost, "http://x/", nil)
	req.Body = failReader{}
	LogHTTPRequest(req)
	LogHTTPResponse(&http.Response{Status: "500", StatusCode: http.StatusInternalServerError, Header: http.Header{}, Body: failReader{}})
	out := buf.String()
	if !strings.Contains(out, "Error reading request body: broken pipe") || !strings.Contains(out, "Error reading response body: broken pipe") {
		t.Fatalf("log = %q", out)
	}
}

// CloseQuietly logs a close error only in debug mode.
func TestCloseQuietly(t *testing.T) {
	buf := captureLog(t)
	CloseQuietly(failReader{})
	if !strings.Contains(buf.String(), "close failed: close failed") {
		t.Fatalf("log = %q", buf.String())
	}
	buf.Reset()
	Debug = false
	CloseQuietly(failReader{})
	if buf.Len() != 0 {
		t.Fatalf("logged with Debug off: %q", buf.String())
	}
}
