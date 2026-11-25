package mcpremote

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/onegator/gator/internal/server/untrusted"
)

// DefaultMaxResult bounds what one tool's answer may put in an agent's context.
const DefaultMaxResult = 64 << 10

// fenced is an Upstream whose answers come back as data: every text a tool returns is in the
// untrusted fence (a nonce made for that answer, anything shaped like a fence defused), within
// a size limit, and nothing comes back outside it. What a remote server says was written outside
// this workspace, and may carry instructions; the agent reads it, it does not obey it.
type fenced struct {
	Upstream
	who string // whose words: the server's display name
	max int
}

func (u fenced) CallTool(ctx context.Context, name string, args json.RawMessage) (*sdk.CallToolResult, error) {
	res, err := u.Upstream.CallTool(ctx, name, args)
	if err != nil || res == nil {
		return res, err
	}
	return Fence(res, fmt.Sprintf("%s (tool %s)", u.who, name), u.max), nil
}

// Fence is a tool's answer as an agent gets it: its texts (and the texts of resources it
// embeds) in one untrusted fence, cut at max bytes; images, audio and structured content left
// out with a word, since none of them can be fenced.
func Fence(res *sdk.CallToolResult, who string, max int) *sdk.CallToolResult {
	if max <= 0 {
		max = DefaultMaxResult
	}
	var b strings.Builder
	left := 0
	for _, c := range res.Content {
		switch v := c.(type) {
		case *sdk.TextContent:
			add(&b, v.Text)
		case *sdk.EmbeddedResource:
			if v.Resource != nil && v.Resource.Text != "" {
				add(&b, v.Resource.Text)
			} else {
				left++
			}
		case *sdk.ResourceLink:
			add(&b, v.URI)
		default:
			left++
		}
	}
	if res.StructuredContent != nil && b.Len() == 0 {
		if raw, err := json.Marshal(res.StructuredContent); err == nil {
			add(&b, string(raw))
		}
	}
	text := b.String()
	if len(text) > max {
		text = strings.ToValidUTF8(text[:max], "") + fmt.Sprintf("\n[… cut at %d bytes]", max)
	}
	note := ""
	if left > 0 {
		note = fmt.Sprintf("\n(%d parts of the answer that are not text were left out)", left)
	}
	return &sdk.CallToolResult{IsError: res.IsError, Content: []sdk.Content{&sdk.TextContent{Text: untrusted.New().Fence(who, text) + note}}}
}

func add(b *strings.Builder, s string) {
	if b.Len() > 0 {
		b.WriteString("\n")
	}
	b.WriteString(s)
}
