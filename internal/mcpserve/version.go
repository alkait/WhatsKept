package mcpserve

import (
	"context"
	"errors"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Version is the release this server reports — set by main from the
// stamped binary version.
var Version = "dev"

// A client keeps the tool list and instructions it got when its
// conversation began, so a conversation that outlives an upgrade talks
// to the new server with the old tools. Two signals let the agent tell
// the user: the instructions name the version the conversation began
// with, and every tool result ends with the version serving it — the
// instructions ask the agent to compare them. A call the new server no
// longer accepts gets the same advice as its error.

// versionedInstructions fills the version into the embedded
// instructions.
func versionedInstructions() string {
	return strings.ReplaceAll(instructions, "{{version}}", Version)
}

// addTool registers a tool whose output schema allows fields it does
// not name: a client checks results against the schema it got when its
// conversation began, so a field a later release adds must not make an
// older conversation reject every result.
func addTool[In, Out any](s *mcp.Server, t *mcp.Tool, h mcp.ToolHandlerFor[In, Out]) {
	schema, err := jsonschema.For[Out](nil)
	if err != nil {
		panic(err)
	}
	allowExtraFields(schema)
	t.OutputSchema = schema
	// A client may keep its tool list longer than its instructions, so
	// each description names the version it came from too.
	t.Description += " (whatskept " + Version + ")"
	mcp.AddTool(s, t, h)
}

// allowExtraFields lifts additionalProperties: false throughout a
// schema.
func allowExtraFields(s *jsonschema.Schema) {
	if s == nil {
		return
	}
	s.AdditionalProperties = nil
	for _, p := range s.Properties {
		allowExtraFields(p)
	}
	for _, d := range s.Defs {
		allowExtraFields(d)
	}
	allowExtraFields(s.Items)
}

// staleTools is the advice for a call the running server no longer
// accepts.
func staleTools() string {
	return "this conversation's whatskept tools are out of date — the server is now " + Version +
		". Tell the user to reconnect the whatskept connector and start a new conversation."
}

// versionMiddleware stamps the version on every successful tool
// result and turns calls to removed tools, or with arguments the
// current tools reject, into the reconnect advice.
func versionMiddleware(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		res, err := next(ctx, method, req)
		if method != "tools/call" {
			return res, err
		}
		var rpcErr *jsonrpc.Error
		if errors.As(err, &rpcErr) && strings.HasPrefix(rpcErr.Message, "unknown tool") {
			out := &mcp.CallToolResult{}
			out.SetError(errors.New(staleTools() + " (" + rpcErr.Message + ")"))
			return out, nil
		}
		r, ok := res.(*mcp.CallToolResult)
		if err != nil || !ok {
			return res, err
		}
		if r.IsError {
			// The SDK's own wording for arguments failing the tool's
			// input schema.
			if text := resultText(r); strings.HasPrefix(text, `validating "arguments"`) {
				r.Content = []mcp.Content{&mcp.TextContent{Text: staleTools() + " (" + text + ")"}}
			}
			return r, nil
		}
		stampVersion(r)
		return r, nil
	}
}

// stampVersion adds the version as a text block of its own, beside the
// result — not inside the structured content, which an older
// conversation checks against the schema it began with.
func stampVersion(r *mcp.CallToolResult) {
	r.Content = append(r.Content, &mcp.TextContent{Text: "server_version: " + Version})
}

func resultText(r *mcp.CallToolResult) string {
	var parts []string
	for _, c := range r.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			parts = append(parts, tc.Text)
		}
	}
	return strings.Join(parts, " ")
}
