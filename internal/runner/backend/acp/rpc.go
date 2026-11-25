package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
)

// maxLine bounds one JSON-RPC message from the agent: a whole file read back, at most.
const maxLine = 32 << 20

// JSON-RPC error codes this client answers with.
const (
	codeMethodNotFound = -32601
	codeInternal       = -32603
)

// RPCError is a JSON-RPC error, from the agent or for it.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string { return fmt.Sprintf("acp %d: %s", e.Code, e.Message) }

// errClosed is a call on a connection whose agent went away.
var errClosed = errors.New("acp: the agent closed its connection")

// handler serves what the agent sends: a request (id set) answers a result, a notification
// (id empty) answers nothing. Both are served in the order they arrive, on the reading
// goroutine, because the order of session/update is the order of the agent's words.
type handler func(method string, params json.RawMessage, request bool) (any, error)

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// conn is one JSON-RPC 2.0 connection over newline-delimited JSON, the transport ACP uses over
// an agent's stdio. Unlike the plugin connection, it serves what comes in one message at a time.
type conn struct {
	w   io.Writer
	wmu sync.Mutex
	h   handler

	mu      sync.Mutex
	next    int64
	pending map[string]chan rpcMessage
	done    chan struct{}
	err     error
	bad     int // lines that were not JSON-RPC
}

func newConn(r io.Reader, w io.Writer, h handler) *conn {
	c := &conn{w: w, h: h, pending: map[string]chan rpcMessage{}, done: make(chan struct{})}
	go c.read(r)
	return c
}

func (c *conn) read(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), maxLine)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var m rpcMessage
		if err := json.Unmarshal(line, &m); err != nil {
			c.mu.Lock()
			c.bad++
			c.mu.Unlock()
			continue // an agent's stray log line on stdout is not worth ending the session for
		}
		if m.Method != "" {
			c.serve(m)
			continue
		}
		c.mu.Lock()
		ch := c.pending[string(m.ID)]
		delete(c.pending, string(m.ID))
		c.mu.Unlock()
		if ch != nil {
			ch <- m
		}
	}
	err := sc.Err()
	if err == nil {
		err = errClosed
	}
	c.mu.Lock()
	c.err = err
	for id, ch := range c.pending {
		close(ch)
		delete(c.pending, id)
	}
	c.mu.Unlock()
	close(c.done)
}

func (c *conn) serve(m rpcMessage) {
	request := len(m.ID) > 0
	result, err := c.h(m.Method, m.Params, request)
	if !request {
		return
	}
	resp := rpcMessage{JSONRPC: "2.0", ID: m.ID}
	if err != nil {
		re, ok := errors.AsType[*RPCError](err)
		if !ok {
			re = &RPCError{Code: codeInternal, Message: err.Error()}
		}
		resp.Error = re
	} else {
		b, merr := json.Marshal(result)
		if merr != nil {
			resp.Error = &RPCError{Code: codeInternal, Message: merr.Error()}
		} else {
			resp.Result = b
		}
	}
	_ = c.write(resp)
}

func (c *conn) write(m rpcMessage) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err = c.w.Write(b)
	return err
}

// call sends a request and decodes its result into out (which may be nil).
func (c *conn) call(ctx context.Context, method string, params, out any) error {
	p, err := json.Marshal(params)
	if err != nil {
		return err
	}
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		return errClosed
	}
	c.next++
	id := json.RawMessage(strconv.FormatInt(c.next, 10))
	ch := make(chan rpcMessage, 1)
	c.pending[string(id)] = ch
	c.mu.Unlock()
	if err := c.write(rpcMessage{JSONRPC: "2.0", ID: id, Method: method, Params: p}); err != nil {
		c.forget(id)
		return fmt.Errorf("%w: %v", errClosed, err)
	}
	select {
	case <-ctx.Done():
		c.forget(id)
		return ctx.Err()
	case m, ok := <-ch:
		if !ok {
			return errClosed
		}
		if m.Error != nil {
			return m.Error
		}
		if out != nil && len(m.Result) > 0 && string(m.Result) != "null" {
			return json.Unmarshal(m.Result, out)
		}
		return nil
	}
}

func (c *conn) forget(id json.RawMessage) {
	c.mu.Lock()
	delete(c.pending, string(id))
	c.mu.Unlock()
}

// notify sends a notification.
func (c *conn) notify(method string, params any) error {
	p, err := json.Marshal(params)
	if err != nil {
		return err
	}
	return c.write(rpcMessage{JSONRPC: "2.0", Method: method, Params: p})
}

// badLines is how many lines from the agent were not JSON.
func (c *conn) badLines() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bad
}
