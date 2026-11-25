package mcpserve

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Version is the release this server reports — set by main from the
// stamped binary version.
var Version = "dev"

// A client keeps the tool list it got earlier — claude.ai well past
// an upgrade, even into new conversations — so a conversation can talk
// to the new server with old tools. The server finds out by asking:
// every tool is advertised with a required tools_version argument
// whose only allowed value is this release, so a call made from an
// older tool list arrives with an older value or none. The call is
// served all the same, and the result ends with a notice for the agent
// to pass on. A call the new server no longer accepts gets the same
// advice as its error.

// toolsVersionArg is the argument every advertised tool carries.
const toolsVersionArg = "tools_version"

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

// howToUpdate is what a user does about a conversation on older
// tools. claude.ai hands new conversations a saved tool list that
// catches up minutes after an upgrade, sooner once the connector's
// tool list is refreshed.
const howToUpdate = "start a new conversation to use the update; if a new conversation says this too, " +
	"refresh the whatskept connector's tool list (or reconnect it) and start another in a few minutes."

// staleTools is the advice for a call the running server no longer
// accepts.
func staleTools() string {
	return "this conversation's whatskept tools are out of date — the server is now " + Version +
		". Tell the user to " + howToUpdate
}

// versionMiddleware advertises tools_version on every tool, takes it
// back off each call before the tool sees it, ends every successful
// result with the serving version — and a notice when the call came
// from an older tool list — and turns calls to removed tools, or with
// arguments the current tools reject, into the reconnect advice.
func versionMiddleware(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		called := ""
		if call, ok := req.(*mcp.CallToolRequest); ok && method == "tools/call" {
			called = takeToolsVersion(call.Params)
		}
		res, err := next(ctx, method, req)
		if list, ok := res.(*mcp.ListToolsResult); ok && err == nil {
			return advertiseToolsVersion(list), nil
		}
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
		// A text block of its own, beside the result — not inside the
		// structured content, which an older conversation checks
		// against the schema it began with.
		note := "server_version: " + Version
		if called != Version {
			from := "an earlier version"
			if called != "" {
				from = called
			}
			note += "\nNOTICE: this conversation is using whatskept tools from " + from +
				", but whatskept has been updated to " + Version + ". The result above is valid. " +
				"Tell the user, once in this conversation: whatskept was updated; " + howToUpdate
		}
		r.Content = append(r.Content, &mcp.TextContent{Text: note})
		return r, nil
	}
}

// takeToolsVersion removes tools_version from a call's arguments and
// returns it — "" when the call carries none.
func takeToolsVersion(p *mcp.CallToolParamsRaw) string {
	var args map[string]json.RawMessage
	if p == nil || json.Unmarshal(p.Arguments, &args) != nil {
		return ""
	}
	raw, ok := args[toolsVersionArg]
	if !ok {
		return ""
	}
	delete(args, toolsVersionArg)
	p.Arguments, _ = json.Marshal(args)
	var v string
	json.Unmarshal(raw, &v)
	return v
}

// advertiseToolsVersion returns the tool list with tools_version added
// to every input schema, as a required argument.
func advertiseToolsVersion(list *mcp.ListToolsResult) *mcp.ListToolsResult {
	out := *list
	out.Tools = make([]*mcp.Tool, len(list.Tools))
	for i, t := range list.Tools {
		tool := *t
		var schema map[string]any
		raw, _ := json.Marshal(t.InputSchema)
		json.Unmarshal(raw, &schema)
		props, _ := schema["properties"].(map[string]any)
		if props == nil {
			props = map[string]any{}
		}
		props[toolsVersionArg] = map[string]any{
			"type": "string", "enum": []string{Version},
			"description": "always pass exactly " + Version,
		}
		schema["properties"] = props
		required, _ := schema["required"].([]any)
		schema["required"] = append(required, toolsVersionArg)
		tool.InputSchema = schema
		out.Tools[i] = &tool
	}
	return &out
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
