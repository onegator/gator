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

// Client is the slice of Sentry's API this plugin uses: one call, to resolve an issue.
type Client struct {
	Base  string
	Token string
	HTTP  *http.Client
}

// APIError is a non-2xx answer, kept whole so the caller can decide what it means.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("sentry: http %d", e.Status)
	}
	return fmt.Sprintf("sentry: http %d: %s", e.Status, e.Message)
}

// resolve marks an issue resolved. Sentry accepts this for an issue already resolved, so a
// repeat is not an error.
func (c *Client) resolve(ctx context.Context, issueID string) error {
	body, _ := json.Marshal(map[string]string{"status": "resolved"})
	req, err := http.NewRequestWithContext(ctx, http.MethodPut,
		c.Base+"/issues/"+issueID+"/", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	res, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode/100 == 2 {
		return nil
	}
	// Sentry answers errors as JSON, but a proxy in front of it may not; either way the first
	// line is what a person needs to see.
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 4<<10))
	msg := strings.TrimSpace(string(raw))
	var problem struct {
		Detail string `json:"detail"`
	}
	if json.Unmarshal(raw, &problem) == nil && problem.Detail != "" {
		msg = problem.Detail
	}
	return &APIError{Status: res.StatusCode, Message: msg}
}
