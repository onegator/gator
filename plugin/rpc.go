package plugin

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

// JSON-RPC error codes. -32000 and below are Gator's own.
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternal       = -32603
	CodeNotAvailable   = -32001 // method exists in the protocol but not in this core yet
	CodeForbidden      = -32003 // outside the plugin's project
	CodeNotFound       = -32004
)

// MaxMessageBytes bounds one message; webhook bodies are limited to 1 MiB before encoding.
const MaxMessageBytes = 8 << 20

// Error is a JSON-RPC error.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return fmt.Sprintf("rpc %d: %s", e.Code, e.Message) }

// Errorf builds an Error.
func Errorf(code int, format string, a ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, a...)}
}

// ErrClosed is returned for calls on a connection whose peer went away.
var ErrClosed = errors.New("plugin connection closed")

// Handler serves incoming requests. A nil result is sent as JSON null.
type Handler func(ctx context.Context, method string, params json.RawMessage) (any, error)

type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// Conn is one bidirectional JSON-RPC 2.0 connection over newline-delimited JSON. Both sides
// can call; each incoming request is served on its own goroutine.
type Conn struct {
	w       io.Writer
	wmu     sync.Mutex
	handler Handler

	mu      sync.Mutex
	next    int64
	pending map[string]chan message
	err     error

	ctx    context.Context
	cancel context.CancelFunc
}

// NewConn starts reading r. Requests go to h (nil answers every request with method not found).
func NewConn(r io.Reader, w io.Writer, h Handler) *Conn {
	ctx, cancel := context.WithCancel(context.Background())
	c := &Conn{w: w, handler: h, pending: map[string]chan message{}, ctx: ctx, cancel: cancel}
	go c.read(r)
	return c
}

// Done is closed when the peer's stream ends.
func (c *Conn) Done() <-chan struct{} { return c.ctx.Done() }

// Err is why the connection ended.
func (c *Conn) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

func (c *Conn) read(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), MaxMessageBytes)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var m message
		if err := json.Unmarshal(line, &m); err != nil {
			_ = c.write(message{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: Errorf(CodeParseError, "parse error: %v", err)})
			continue
		}
		if m.Method != "" {
			go c.serve(m)
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
		err = ErrClosed
	}
	c.mu.Lock()
	c.err = err
	for id, ch := range c.pending {
		close(ch)
		delete(c.pending, id)
	}
	c.mu.Unlock()
	c.cancel()
}

func (c *Conn) serve(m message) {
	var result any
	var err error
	func() {
		defer func() {
			if p := recover(); p != nil {
				err = Errorf(CodeInternal, "panic: %v", p)
			}
		}()
		if c.handler == nil {
			err = Errorf(CodeMethodNotFound, "method %q not found", m.Method)
			return
		}
		result, err = c.handler(c.ctx, m.Method, m.Params)
	}()
	if len(m.ID) == 0 {
		return // notification
	}
	resp := message{JSONRPC: "2.0", ID: m.ID}
	if err != nil {
		var re *Error
		if !errors.As(err, &re) {
			re = Errorf(CodeInternal, "%s", err.Error())
		}
		resp.Error = re
	} else {
		b, merr := json.Marshal(result)
		if merr != nil {
			resp.Error = Errorf(CodeInternal, "encode result: %v", merr)
		} else {
			resp.Result = b
		}
	}
	_ = c.write(resp)
}

func (c *Conn) write(m message) error {
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

// Call sends a request and decodes the result into out (which may be nil).
func (c *Conn) Call(ctx context.Context, method string, params, out any) error {
	p, err := json.Marshal(params)
	if err != nil {
		return err
	}
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		return ErrClosed
	}
	c.next++
	id := json.RawMessage(strconv.FormatInt(c.next, 10))
	ch := make(chan message, 1)
	c.pending[string(id)] = ch
	c.mu.Unlock()
	if err := c.write(message{JSONRPC: "2.0", ID: id, Method: method, Params: p}); err != nil {
		c.forget(id)
		return fmt.Errorf("%w: %v", ErrClosed, err)
	}
	select {
	case <-ctx.Done():
		c.forget(id)
		return ctx.Err()
	case m, ok := <-ch:
		if !ok {
			return ErrClosed
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

func (c *Conn) forget(id json.RawMessage) {
	c.mu.Lock()
	delete(c.pending, string(id))
	c.mu.Unlock()
}

// Notify sends a request without waiting for (or getting) an answer.
func (c *Conn) Notify(method string, params any) error {
	p, err := json.Marshal(params)
	if err != nil {
		return err
	}
	return c.write(message{JSONRPC: "2.0", Method: method, Params: p})
}
