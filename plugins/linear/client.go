package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Client is the slice of Linear's GraphQL API the plugin uses.
type Client struct {
	URL  string
	Key  string
	HTTP *http.Client
}

// APIError is an answer that is not a success: an HTTP status, or GraphQL errors with a 200.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string { return fmt.Sprintf("linear: http %d: %s", e.Status, e.Message) }

func (c *Client) do(ctx context.Context, query string, vars map[string]any, out any) error {
	body, _ := json.Marshal(map[string]any{"query": query, "variables": vars})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	// Linear's personal keys go in the header as they are, without "Bearer".
	req.Header.Set("Authorization", c.Key)
	req.Header.Set("Content-Type", "application/json")
	res, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode/100 != 2 {
		return &APIError{Status: res.StatusCode, Message: strings.TrimSpace(string(raw))}
	}
	var env struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message    string `json:"message"`
			Extensions struct {
				Code string `json:"code"`
			} `json:"extensions"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("linear: unreadable answer: %w", err)
	}
	if len(env.Errors) > 0 {
		// GraphQL reports a refused key as an error with 200; treat it as the 401 it is.
		status := http.StatusBadRequest
		if env.Errors[0].Extensions.Code == "AUTHENTICATION_ERROR" {
			status = http.StatusUnauthorized
		}
		return &APIError{Status: status, Message: env.Errors[0].Message}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(env.Data, out)
}

// stateID finds the id of the named workflow state in the issue's team. Names are what a person
// writes in state_map; ids are what the API wants. Empty when the team has no such state.
func (c *Client) stateID(ctx context.Context, issueID, name string) (string, error) {
	var out struct {
		Issue struct {
			Team struct {
				States struct {
					Nodes []struct {
						ID   string `json:"id"`
						Name string `json:"name"`
					} `json:"nodes"`
				} `json:"states"`
			} `json:"team"`
		} `json:"issue"`
	}
	const q = `query($id: String!) { issue(id: $id) { team { states { nodes { id name } } } } }`
	if err := c.do(ctx, q, map[string]any{"id": issueID}, &out); err != nil {
		return "", err
	}
	for _, s := range out.Issue.Team.States.Nodes {
		if strings.EqualFold(s.Name, name) {
			return s.ID, nil
		}
	}
	return "", nil
}

func (c *Client) moveIssue(ctx context.Context, issueID, stateID string) error {
	const q = `mutation($id: String!, $state: String!) { issueUpdate(id: $id, input: {stateId: $state}) { success } }`
	return c.do(ctx, q, map[string]any{"id": issueID, "state": stateID}, nil)
}
