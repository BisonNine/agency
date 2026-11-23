package stubprovider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type chatResponse struct {
	Choices []struct {
		FinishReason string `json:"finish_reason"`
		Message      struct {
			Content   *string `json:"content"`
			ToolCalls []struct {
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
	} `json:"choices"`
}

// reply is a response with its body read.
type reply struct {
	StatusCode int
	Header     http.Header
}

// post sends a chat request and returns the response status, headers and body.
func post(t *testing.T, ctx context.Context, s *Server, body string) (reply, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.URL()+"/chat/completions", strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer alice")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer closeBody(t, resp)
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return reply{StatusCode: resp.StatusCode, Header: resp.Header}, b
}

func closeBody(t *testing.T, resp *http.Response) {
	t.Helper()
	if err := resp.Body.Close(); err != nil {
		t.Error(err)
	}
}

const simple = `{"model":"m","messages":[{"role":"user","content":"hi"}]}`

func TestModelsEndpoint(t *testing.T) {
	s := New(t)
	resp, err := http.Get(s.URL() + "/models")
	require.NoError(t, err)
	defer closeBody(t, resp)
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.JSONEq(t, `{"object":"list","data":[{"id":"stub","object":"model"}]}`, string(b))
}

func TestDefaultReplyAndCapture(t *testing.T) {
	s := New(t)
	body := `{"model":"m","stream":false,"tools":[{},{}],"response_format":{"type":"json_object"},` +
		`"messages":[{"role":"system","content":"be brief"},{"role":"user","content":[{"type":"text","text":"look "},{"type":"image_url"}]},{"role":"assistant","content":null}]}`
	resp, b := post(t, context.Background(), s, body)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var cr chatResponse
	require.NoError(t, json.Unmarshal(b, &cr))
	require.Len(t, cr.Choices, 1)
	assert.Equal(t, "OK", *cr.Choices[0].Message.Content)
	assert.Equal(t, "stop", cr.Choices[0].FinishReason)

	require.Equal(t, 1, s.Count())
	r := s.Requests()[0]
	assert.Equal(t, "m", r.Model)
	assert.False(t, r.Stream)
	assert.Equal(t, 2, r.Tools)
	assert.JSONEq(t, `{"type":"json_object"}`, string(r.ResponseFormat))
	assert.Equal(t, "Bearer alice", r.Header.Get("Authorization"))
	assert.Equal(t, body, string(r.Body))
	assert.Equal(t, []Message{{"system", "be brief"}, {"user", "look [image_url]"}, {"assistant", ""}}, r.Messages)
	assert.Equal(t, "be brief\nlook [image_url]\n\n", r.Text())
}

func TestScriptThenDefault(t *testing.T) {
	s := New(t)
	s.Script(Reply("one"), CallTool("lookup", ""), CallTool("find", `{"q":"x"}`))
	s.SetDefault(Reply("after"))
	assert.Equal(t, 3, s.Remaining())

	_, b := post(t, context.Background(), s, simple)
	var cr chatResponse
	require.NoError(t, json.Unmarshal(b, &cr))
	assert.Equal(t, "one", *cr.Choices[0].Message.Content)

	for _, want := range []struct{ name, args string }{{"lookup", "{}"}, {"find", `{"q":"x"}`}} {
		_, b = post(t, context.Background(), s, simple)
		cr = chatResponse{}
		require.NoError(t, json.Unmarshal(b, &cr))
		c := cr.Choices[0]
		assert.Equal(t, "tool_calls", c.FinishReason)
		assert.Nil(t, c.Message.Content)
		require.Len(t, c.Message.ToolCalls, 1)
		assert.Equal(t, "call_"+want.name, c.Message.ToolCalls[0].ID)
		assert.Equal(t, want.name, c.Message.ToolCalls[0].Function.Name)
		assert.Equal(t, want.args, c.Message.ToolCalls[0].Function.Arguments)
	}
	assert.Equal(t, 0, s.Remaining())

	_, b = post(t, context.Background(), s, simple)
	cr = chatResponse{}
	require.NoError(t, json.Unmarshal(b, &cr))
	assert.Equal(t, "after", *cr.Choices[0].Message.Content)
}

func TestToolCallWithoutArguments(t *testing.T) {
	s := New(t)
	s.Script(Step{ToolCalls: []ToolCall{{ID: "c1", Name: "noargs"}}})
	_, b := post(t, context.Background(), s, simple)
	var cr chatResponse
	require.NoError(t, json.Unmarshal(b, &cr))
	assert.Equal(t, "{}", cr.Choices[0].Message.ToolCalls[0].Function.Arguments)
}

func TestHookOverridesScript(t *testing.T) {
	s := New(t)
	s.Script(Reply("scripted"))
	s.SetHook(func(req Request) *Step {
		if strings.Contains(req.Text(), "alice") {
			st := Reply("hooked")
			return &st
		}
		return nil
	})
	_, b := post(t, context.Background(), s, `{"model":"m","messages":[{"role":"user","content":"alice"}]}`)
	assert.Contains(t, string(b), `"hooked"`)
	assert.Equal(t, 1, s.Remaining(), "the hook does not consume the script")
	_, b = post(t, context.Background(), s, simple)
	assert.Contains(t, string(b), `"scripted"`)
}

func TestErrorShapes(t *testing.T) {
	s := New(t)
	s.Script(
		StatusError(http.StatusTeapot, "short and stout"),
		RateLimited(1500*time.Millisecond),
		ServerError(http.StatusBadGateway),
		PayloadTooLarge(),
		ContextLengthExceeded(),
		Unauthorized(),
		Malformed(),
		Step{Status: http.StatusOK, Content: "explicit 200", Header: map[string]string{"X-Alice": "1"}},
	)
	tests := []struct {
		status int
		body   string
		header [2]string
	}{
		{http.StatusTeapot, `{"error":{"message":"short and stout","type":"server_error"}}`, [2]string{}},
		{http.StatusTooManyRequests, `{"error":{"code":"rate_limit_exceeded","message":"Rate limit reached for requests","type":"requests"}}`, [2]string{"Retry-After", "2"}},
		{http.StatusBadGateway, `{"error":{"message":"The server had an error processing your request","type":"server_error"}}`, [2]string{}},
		{http.StatusRequestEntityTooLarge, `{"error":{"message":"Request too large","type":"invalid_request_error"}}`, [2]string{}},
		{http.StatusBadRequest, `{"error":{"code":"context_length_exceeded","message":"This model's maximum context length is 8192 tokens. However, your messages resulted in 9000 tokens.","type":"invalid_request_error"}}`, [2]string{}},
		{http.StatusUnauthorized, `{"error":{"code":"invalid_api_key","message":"Incorrect API key provided","type":"invalid_request_error"}}`, [2]string{}},
	}
	for _, tt := range tests {
		resp, b := post(t, context.Background(), s, simple)
		assert.Equal(t, tt.status, resp.StatusCode)
		assert.JSONEq(t, tt.body, string(b))
		if tt.header[0] != "" {
			assert.Equal(t, tt.header[1], resp.Header.Get(tt.header[0]))
		}
	}
	resp, b := post(t, context.Background(), s, simple)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "<html>not json</html>", string(b))

	resp, b = post(t, context.Background(), s, simple)
	assert.Equal(t, "1", resp.Header.Get("X-Alice"))
	assert.Contains(t, string(b), `"explicit 200"`)
}

func TestBadRequestBody(t *testing.T) {
	s := New(t)
	resp, b := post(t, context.Background(), s, "{not json")
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	assert.True(t, strings.HasPrefix(string(b), "bad json: "), string(b))
	assert.Equal(t, 0, s.Count())
}

// sseEvents returns the data payloads of an event stream.
func sseEvents(t *testing.T, b []byte) []string {
	t.Helper()
	var out []string
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		if data, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
			out = append(out, data)
		}
	}
	return out
}

func TestStreaming(t *testing.T) {
	s := New(t)
	s.Script(Reply("streamed"), CallTool("lookup", `{"q":1}`))
	stream := `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`

	resp, b := post(t, context.Background(), s, stream)
	assert.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))
	ev := sseEvents(t, b)
	require.Len(t, ev, 4)
	assert.Contains(t, ev[0], `"content":"streamed"`)
	assert.Contains(t, ev[1], `"finish_reason":"stop"`)
	assert.Contains(t, ev[2], `"usage"`)
	assert.Equal(t, "[DONE]", ev[3])
	assert.True(t, s.Requests()[0].Stream)

	_, b = post(t, context.Background(), s, stream)
	ev = sseEvents(t, b)
	require.Len(t, ev, 4)
	assert.Contains(t, ev[0], `"tool_calls":[{"function":{"arguments":"{\"q\":1}","name":"lookup"},"id":"call_lookup","index":0,"type":"function"}]`)
	assert.Contains(t, ev[1], `"finish_reason":"tool_calls"`)
}

func TestHangWithLimit(t *testing.T) {
	s := New(t)
	s.Script(Hang(time.Millisecond))
	resp, _ := post(t, context.Background(), s, simple)
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	assert.Equal(t, 0, s.Hanging())
}

// A hang without a limit is held until the client gives up.
func TestHangUntilClientCancels(t *testing.T) {
	s := New(t)
	s.Script(Hang(0))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.URL()+"/chat/completions", strings.NewReader(simple))
		if err != nil {
			done <- err
			return
		}
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			err = resp.Body.Close()
		}
		done <- err
	}()
	require.Eventually(t, func() bool { return s.Hanging() == 1 }, 10*time.Second, time.Millisecond)
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
	require.Eventually(t, func() bool { return s.Hanging() == 0 }, 10*time.Second, time.Millisecond)
}

func TestFlattenContent(t *testing.T) {
	assert.Equal(t, "", flattenContent(nil))
	assert.Equal(t, "", flattenContent(json.RawMessage("null")))
	assert.Equal(t, "hi", flattenContent(json.RawMessage(`"hi"`)))
	assert.Equal(t, "a[image_url]", flattenContent(json.RawMessage(`[{"type":"text","text":"a"},{"type":"image_url"},{}]`)))
	assert.Equal(t, "42", flattenContent(json.RawMessage(`42`)))
}

func TestMustJSONPanicsOnUnmarshalable(t *testing.T) {
	assert.Panics(t, func() { mustJSON(make(chan int)) })
}

// failingWriter fails every write.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestWriteStringLogsFailure(t *testing.T) {
	writeString(failingWriter{}, "x") // logged, not fatal
}
