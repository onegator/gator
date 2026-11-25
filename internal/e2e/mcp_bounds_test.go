package e2e

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/onegator/gator/internal/server/api/gen"
)

// What a remote server says is bounded before anything of it is used: an answer larger than the
// proxy reads is refused rather than parsed, a tool whose schema is huge or whose name is not one
// is not offered, and a title is cut to a line.
func TestTheProxyBoundsWhatAServerSays(t *testing.T) {
	h := newHarness(t)
	s := h.remoteSetup()
	srv := sdk.NewServer(&sdk.Implementation{Name: "big", Version: "1"}, nil)
	ro := &sdk.ToolAnnotations{ReadOnlyHint: true}
	type none struct{}
	sdk.AddTool(srv, &sdk.Tool{Name: "small", Title: strings.Repeat("long title ", 30), Annotations: ro}, func(context.Context, *sdk.CallToolRequest, none) (*sdk.CallToolResult, any, error) {
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "fine"}}}, nil, nil
	})
	sdk.AddTool(srv, &sdk.Tool{Name: "flood", Annotations: ro}, func(context.Context, *sdk.CallToolRequest, none) (*sdk.CallToolResult, any, error) {
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: strings.Repeat("x", 6<<20)}}}, nil, nil
	})
	props := map[string]*jsonschema.Schema{}
	for i := range 2000 {
		props[strings.Repeat("p", 10)+string(rune('a'+i%26))+strings.Repeat("q", i%40)] = &jsonschema.Schema{Type: "string", Description: strings.Repeat("d", 20)}
	}
	srv.AddTool(&sdk.Tool{Name: "huge", InputSchema: &jsonschema.Schema{Type: "object", Properties: props}, Annotations: ro},
		func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			return &sdk.CallToolResult{}, nil
		})
	big := httptest.NewServer(sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return srv }, &sdk.StreamableHTTPOptions{Stateless: true, JSONResponse: true}))
	defer big.Close()
	allowed := []string{"small", "flood", "huge"}
	if code := h.do("PUT", "/projects/"+s.project+"/mcp-servers/big", gen.McpServerSettings{Url: ptr(big.URL + "/mcp"), Auth: ptr(gen.McpServerSettingsAuthNone),
		AllowedTools: &allowed, ReadOnlyTools: &allowed}, nil); code != 200 {
		t.Fatalf("server: %d", code)
	}
	if code := h.do("PATCH", "/agents/"+s.agent.Id.String(), gen.AgentPatch{Tools: &[]string{"issues-mcp", "big"}}, nil); code != 200 {
		t.Fatalf("give it: %d", code)
	}
	_, job := h.replyJobOf(s.project, "oncall")
	cs, err := h.proxySession(h.srv.URL+"/mcp/proxy/big", map[string]string{"Authorization": "Bearer " + h.identity(job, s.project)})
	if err != nil {
		t.Fatal(err)
	}
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
		if len(tool.Title) > 90 {
			t.Fatalf("a title of %d bytes", len(tool.Title))
		}
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"flood", "small"}) {
		t.Fatalf("offered: %v", names)
	}
	out, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: "flood", Arguments: map[string]any{}})
	if err != nil || !out.IsError {
		t.Fatalf("a 6 MiB answer: %v %+v", err, out)
	}
	if n := h.count(`SELECT count(*) FROM mcp_calls WHERE job_id = $1 AND tool = 'flood' AND outcome = 'error' AND error LIKE '%larger than Gator reads%'`, job); n != 1 {
		t.Fatalf("written down as too large: %d", n)
	}
}
