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
	// Linear's personal keys go in the header as they are; a token from Connect Linear is an
	// OAuth token, which wants "Bearer".
	if strings.HasPrefix(c.Key, "lin_api_") {
		req.Header.Set("Authorization", c.Key)
	} else {
		req.Header.Set("Authorization", "Bearer "+c.Key)
	}
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

// State is a workflow state. Its type is what this plugin reasons with: backlog, unstarted,
// started, completed, canceled or triage. Names are a team's own.
type State struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	Type     string  `json:"type"`
	Position float64 `json:"position"`
}

func (s State) finished() bool { return s.Type == "completed" || s.Type == "canceled" }

// issueStates answers an issue's current state and its team's states.
func (c *Client) issueStates(ctx context.Context, issueID string) (State, []State, error) {
	var out struct {
		Issue struct {
			State State `json:"state"`
			Team  struct {
				States struct {
					Nodes []State `json:"nodes"`
				} `json:"states"`
			} `json:"team"`
		} `json:"issue"`
	}
	const q = `query($id: String!) { issue(id: $id) { state { id name type position } team { states { nodes { id name type position } } } } }`
	if err := c.do(ctx, q, map[string]any{"id": issueID}, &out); err != nil {
		return State{}, nil, err
	}
	return out.Issue.State, out.Issue.Team.States.Nodes, nil
}

// stateID finds the id of the named workflow state in the issue's team. Names are what a person
// writes in state_map; ids are what the API wants. Empty when the team has no such state.
func (c *Client) stateID(ctx context.Context, issueID, name string) (string, error) {
	_, states, err := c.issueStates(ctx, issueID)
	if err != nil {
		return "", err
	}
	for _, s := range states {
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

// apiIssue is an issue as the GraphQL API answers it; labels are a connection there.
type apiIssue struct {
	ID          string `json:"id"`
	Identifier  string `json:"identifier"`
	Title       string `json:"title"`
	Description string `json:"description"`
	URL         string `json:"url"`
	Team        struct {
		Key string `json:"key"`
	} `json:"team"`
	State  State `json:"state"`
	Labels struct {
		Nodes []struct {
			Name string `json:"name"`
		} `json:"nodes"`
	} `json:"labels"`
}

// openIssues lists one page of issues that are not finished, newest first, of one team and
// with one label when those are set.
func (c *Client) openIssues(ctx context.Context, team, label, after string) ([]apiIssue, string, error) {
	filter := map[string]any{"state": map[string]any{"type": map[string]any{"nin": []string{"completed", "canceled"}}}}
	if team != "" {
		filter["team"] = map[string]any{"key": map[string]any{"eqIgnoreCase": team}}
	}
	if label != "" {
		filter["labels"] = map[string]any{"some": map[string]any{"name": map[string]any{"eqIgnoreCase": label}}}
	}
	vars := map[string]any{"filter": filter}
	if after != "" {
		vars["after"] = after
	}
	var out struct {
		Issues struct {
			Nodes    []apiIssue `json:"nodes"`
			PageInfo struct {
				HasNextPage bool   `json:"hasNextPage"`
				EndCursor   string `json:"endCursor"`
			} `json:"pageInfo"`
		} `json:"issues"`
	}
	const q = `query($filter: IssueFilter, $after: String) {
  issues(filter: $filter, first: 100, after: $after, orderBy: createdAt) {
    nodes { id identifier title description url team { key } state { id name type position } labels { nodes { name } } }
    pageInfo { hasNextPage endCursor }
  }
}`
	if err := c.do(ctx, q, vars, &out); err != nil {
		return nil, "", err
	}
	next := ""
	if out.Issues.PageInfo.HasNextPage {
		next = out.Issues.PageInfo.EndCursor
	}
	return out.Issues.Nodes, next, nil
}

// Webhook is a webhook Linear holds.
type Webhook struct {
	ID  string `json:"id"`
	URL string `json:"url"`
}

func (c *Client) webhooks(ctx context.Context) ([]Webhook, error) {
	var out struct {
		Webhooks struct {
			Nodes []Webhook `json:"nodes"`
		} `json:"webhooks"`
	}
	if err := c.do(ctx, `query { webhooks(first: 100) { nodes { id url } } }`, nil, &out); err != nil {
		return nil, err
	}
	return out.Webhooks.Nodes, nil
}

func (c *Client) createWebhook(ctx context.Context, url, secret string) error {
	const q = `mutation($input: WebhookCreateInput!) { webhookCreate(input: $input) { success } }`
	return c.do(ctx, q, map[string]any{"input": map[string]any{"url": url, "secret": secret, "label": "Gator",
		"resourceTypes": []string{"Issue"}, "allPublicTeams": true}}, nil)
}

func (c *Client) updateWebhook(ctx context.Context, id, url, secret string) error {
	const q = `mutation($id: String!, $input: WebhookUpdateInput!) { webhookUpdate(id: $id, input: $input) { success } }`
	return c.do(ctx, q, map[string]any{"id": id, "input": map[string]any{"url": url, "secret": secret,
		"resourceTypes": []string{"Issue"}, "enabled": true}}, nil)
}
