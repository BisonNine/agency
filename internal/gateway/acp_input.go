package gateway

import (
	"bufio"
	"encoding/json"
	"io"
	"sync"

	acplib "github.com/a3tai/openclaw-go/acp"
)

// maxACPLine is the longest JSON-RPC line read from the ACP client, the
// limit the ACP server's own reader has.
const maxACPLine = 10 << 20

// acpInput reads the ACP client's input line by line in its own goroutine.
// The ACP server handles one request at a time and does not read while a
// session/prompt runs, so a session/cancel sent during a prompt would wait
// for the prompt to end. acpInput hands every session/cancel to onCancel as
// soon as it arrives, in input order, and queues every other line for the
// server, which reads them from the returned reader.
func acpInput(in io.Reader, onCancel func(acplib.CancelNotification)) io.Reader {
	q := &lineQueue{}
	q.cond = sync.NewCond(&q.mu)
	go func() {
		sc := bufio.NewScanner(in)
		sc.Buffer(make([]byte, 0, 64*1024), maxACPLine)
		for sc.Scan() {
			line := sc.Bytes()
			if n, ok := cancelNotification(line); ok {
				onCancel(n)
				continue
			}
			q.push(append(append(make([]byte, 0, len(line)+1), line...), '\n'))
		}
		err := sc.Err()
		if err == nil {
			err = io.EOF
		}
		q.close(err)
	}()
	return q
}

// cancelNotification reports whether line is a session/cancel, with its
// parameters (absent or unreadable parameters are the zero value, as the ACP
// server treats them).
func cancelNotification(line []byte) (acplib.CancelNotification, bool) {
	var msg struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	var n acplib.CancelNotification
	if json.Unmarshal(line, &msg) != nil || msg.Method != "session/cancel" {
		return n, false
	}
	if len(msg.Params) > 0 && json.Unmarshal(msg.Params, &n) != nil {
		n = acplib.CancelNotification{} // matches no session
	}
	return n, true
}

// lineQueue is an unbounded queue of input lines read as one stream. Read
// blocks until a line is queued or the input ended.
type lineQueue struct {
	mu    sync.Mutex
	cond  *sync.Cond
	lines [][]byte
	cur   []byte
	err   error // set when the input ended
}

func (q *lineQueue) push(line []byte) {
	q.mu.Lock()
	q.lines = append(q.lines, line)
	q.mu.Unlock()
	q.cond.Signal()
}

func (q *lineQueue) close(err error) {
	q.mu.Lock()
	q.err = err
	q.mu.Unlock()
	q.cond.Broadcast()
}

func (q *lineQueue) Read(p []byte) (int, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.cur) == 0 && len(q.lines) == 0 && q.err == nil {
		q.cond.Wait()
	}
	if len(q.cur) == 0 {
		if len(q.lines) == 0 {
			return 0, q.err
		}
		q.cur, q.lines = q.lines[0], q.lines[1:]
	}
	n := copy(p, q.cur)
	q.cur = q.cur[n:]
	return n, nil
}
