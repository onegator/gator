package a2a

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Client calls one remote agent over JSON-RPC, through an HTTP client the server's outbound policy
// holds (netguard): no private address, no plain http.
type Client struct {
	HTTP *http.Client
}

// FetchCard reads an agent's card. cardURL is the card itself, or the agent's base address, to
// which the well-known path is added.
func (c Client) FetchCard(ctx context.Context, cardURL string) (AgentCard, string, error) {
	u, err := url.Parse(strings.TrimSpace(cardURL))
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil {
		return AgentCard{}, "", fmt.Errorf("%w: the card address is an http(s) address without credentials", ErrInvalid)
	}
	if !strings.HasSuffix(u.Path, ".json") {
		u.Path = strings.TrimRight(u.Path, "/") + CardPath
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return AgentCard{}, "", err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return AgentCard{}, "", fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return AgentCard{}, "", fmt.Errorf("%w: the card answered HTTP %d", ErrUnreachable, resp.StatusCode)
	}
	var card AgentCard
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&card); err != nil {
		return AgentCard{}, "", fmt.Errorf("%w: the card is not JSON: %v", ErrInvalid, err)
	}
	if err := checkCard(card); err != nil {
		return AgentCard{}, "", err
	}
	return card, u.String(), nil
}

// checkCard holds a card to what a client needs of it: a name and a JSON-RPC endpoint.
func checkCard(card AgentCard) error {
	u, err := url.Parse(card.URL)
	switch {
	case strings.TrimSpace(card.Name) == "":
		return fmt.Errorf("%w: the card names no agent", ErrInvalid)
	case err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil:
		return fmt.Errorf("%w: the card's url is not an http(s) endpoint", ErrInvalid)
	case card.PreferredTransport != "" && !strings.EqualFold(card.PreferredTransport, jsonRPCTransportName):
		return fmt.Errorf("%w: the agent prefers %s; Gator speaks JSON-RPC", ErrInvalid, card.PreferredTransport)
	}
	return nil
}

// Errors of calling out.
var (
	ErrInvalid     = errors.New("invalid")
	ErrUnreachable = errors.New("the remote agent could not be reached")
)

// call makes one JSON-RPC call to endpoint.
func (c Client) call(ctx context.Context, endpoint, token, method string, params, out any) error {
	p, err := json.Marshal(params)
	if err != nil {
		return err
	}
	body, _ := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: json.RawMessage(`"` + newID() + `"`), Method: method, Params: p})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: HTTP %d", ErrUnreachable, resp.StatusCode)
	}
	var r rpcResponse
	if err := json.Unmarshal(raw, &r); err != nil {
		return fmt.Errorf("%w: the answer is not JSON-RPC", ErrUnreachable)
	}
	if r.Error != nil {
		return r.Error
	}
	return json.Unmarshal(r.Result, out)
}

// Send gives the agent a message and answers what came back: a task, or a message (a quick answer
// without a task, which A2A allows).
func (c Client) Send(ctx context.Context, endpoint, token string, m Message) (Task, error) {
	var raw json.RawMessage
	if err := c.call(ctx, endpoint, token, methodSend, SendParams{Message: m,
		Configuration: &SendConfig{Blocking: true, AcceptedOutputModes: []string{"text/plain"}}}, &raw); err != nil {
		return Task{}, err
	}
	return readResult(raw)
}

// Get reads a task again.
func (c Client) Get(ctx context.Context, endpoint, token, id string) (Task, error) {
	var raw json.RawMessage
	if err := c.call(ctx, endpoint, token, methodGetTask, TaskQuery{ID: id}, &raw); err != nil {
		return Task{}, err
	}
	return readResult(raw)
}

// readResult reads message/send's and tasks/get's result: a task, or a message that is the answer.
func readResult(raw json.RawMessage) (Task, error) {
	var kind struct {
		Kind string `json:"kind"`
	}
	_ = json.Unmarshal(raw, &kind)
	if kind.Kind == "message" {
		var m Message
		if err := json.Unmarshal(raw, &m); err != nil {
			return Task{}, err
		}
		return Task{Kind: "task", Status: TaskStatus{State: StateCompleted, Message: &m}}, nil
	}
	var t Task
	if err := json.Unmarshal(raw, &t); err != nil {
		return Task{}, err
	}
	if t.Status.State == "" {
		t.Status.State = StateUnknown
	}
	return t, nil
}

func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
