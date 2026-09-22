package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Client is the one call this plugin makes: resolve a fault.
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
		return fmt.Sprintf("honeybadger: http %d", e.Status)
	}
	return fmt.Sprintf("honeybadger: http %d: %s", e.Status, e.Message)
}

// resolve marks a fault resolved. Resolving one already resolved is not an error there.
func (c *Client) resolve(ctx context.Context, project, fault string) error {
	body := []byte(`{"fault":{"resolved":true}}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut,
		c.Base+"/v2/projects/"+project+"/faults/"+fault, bytes.NewReader(body))
	if err != nil {
		return err
	}
	// Honeybadger's API takes the token as the basic-auth user with an empty password.
	req.SetBasicAuth(c.Token, "")
	req.Header.Set("Content-Type", "application/json")
	res, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode/100 == 2 {
		return nil
	}
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 4<<10))
	return &APIError{Status: res.StatusCode, Message: strings.TrimSpace(string(raw))}
}
