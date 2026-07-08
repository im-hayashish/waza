package execution

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	copilot "github.com/github/copilot-sdk/go"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// This file is fork-only. It bridges waza's in-process tools — any request that
// carries copilot.Tool handlers — to the Claude Code CLI, which, being a
// subprocess, cannot invoke in-process Go callbacks directly. The two producers
// of such tools are the prompt/LLM-judge grader (set_waza_grade_pass /
// set_waza_grade_fail / set_pairwise_winner) and the responder loop
// (responder_reply / responder_stop / responder_abstain).
//
// The bridge stands up a tiny in-process MCP server over Streamable HTTP on
// localhost and hands the CLI its URL via --mcp-config. When the model calls a
// tool, the MCP handler routes the call straight back to the originating
// copilot.Tool.Handler closure and returns its result synchronously, so the
// caller's captured state (the grader's Passes/Failures, the responder's
// recorded Decision) populates exactly as it does under the copilot engine — no
// changes to the shared grader/responder code are needed.

// gradeBridgeServerName is the MCP server key the grade tools are exposed under.
// The CLI namespaces MCP tools as mcp__<serverKey>__<tool>, so the judge sees
// e.g. mcp__waza-graders__set_waza_grade_pass. It is referenced by both the
// MCP-config entry (writeMCPConfig) and the judge guidance (buildGradeToolGuidance).
const gradeBridgeServerName = "waza-graders"

// gradeToolBridge is a running in-process MCP-over-HTTP server exposing a set of
// copilot.Tool handlers. Its zero value is not usable; construct with
// startGradeToolBridge and always pair with a deferred shutdown.
type gradeToolBridge struct {
	url     string
	httpSrv *http.Server
}

// startGradeToolBridge stands up the MCP server for tools and begins serving it
// on a random localhost port. Callers must invoke shutdown when the run is done.
func startGradeToolBridge(tools []copilot.Tool) (*gradeToolBridge, error) {
	if len(tools) == 0 {
		return nil, fmt.Errorf("no grade tools to bridge")
	}

	// Stateless mode: each judge run is a single short-lived client, so there is
	// no benefit to server-side session tracking and it removes the Mcp-Session-Id
	// handshake as a possible interop snag with the CLI's MCP client.
	mcpSrv := server.NewMCPServer(gradeBridgeServerName, "1.0.0")
	for _, t := range tools {
		mcpSrv.AddTool(newBridgeTool(t), newBridgeHandler(t))
	}

	streamSrv := server.NewStreamableHTTPServer(mcpSrv, server.WithStateLess(true))

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("failed to listen for grade-tool bridge: %w", err)
	}
	tcpAddr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		_ = ln.Close()
		return nil, fmt.Errorf("grade-tool bridge listener is not TCP: %T", ln.Addr())
	}

	httpSrv := &http.Server{Handler: streamSrv}
	go func() { _ = httpSrv.Serve(ln) }()

	return &gradeToolBridge{
		url:     fmt.Sprintf("http://127.0.0.1:%d/mcp", tcpAddr.Port),
		httpSrv: httpSrv,
	}, nil
}

// shutdown stops the HTTP server (and closes its listener). It is safe to call
// on a nil bridge and to call more than once.
func (b *gradeToolBridge) shutdown() {
	if b == nil || b.httpSrv == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = b.httpSrv.Shutdown(ctx)
}

// newBridgeTool renders a copilot.Tool's declaration (name, description, JSON
// Schema parameters) into an mcp.Tool. The parameter schema is passed through
// verbatim as the MCP raw input schema so the judge sees the same argument
// contract the grader defined.
func newBridgeTool(t copilot.Tool) mcp.Tool {
	schema := json.RawMessage(`{"type":"object","properties":{}}`)
	if len(t.Parameters) > 0 {
		if raw, err := json.Marshal(t.Parameters); err == nil {
			schema = raw
		}
	}
	return mcp.NewToolWithRawSchema(t.Name, t.Description, schema)
}

// newBridgeHandler adapts a copilot.Tool.Handler closure into an mcp-go tool
// handler. The MCP call arguments (a map) are forwarded as the copilot
// ToolInvocation.Arguments, which the grader handlers decode with mapstructure.
func newBridgeHandler(t copilot.Tool) server.ToolHandlerFunc {
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if t.Handler == nil {
			return mcp.NewToolResultText("recorded"), nil
		}
		res, err := t.Handler(copilot.ToolInvocation{
			ToolName:     t.Name,
			Arguments:    request.GetArguments(),
			TraceContext: ctx,
		})
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		text := res.TextResultForLLM
		if strings.TrimSpace(text) == "" {
			text = "recorded"
		}
		return mcp.NewToolResultText(text), nil
	}
}

// buildGradeToolGuidance returns a system-prompt block instructing the model to
// record its result by calling the bridged tools. It serves both producers of
// in-process req.Tools — the prompt/LLM-judge grader (set_waza_grade_*) and the
// responder loop (responder_reply/stop/abstain) — so the wording is deliberately
// neutral ("result"/"decision", not "grade"/"verdict"). Because those tools are
// surfaced through MCP (and therefore namespaced as mcp__<server>__<tool>), and
// because the outcome is captured solely from the tool calls, the model is told
// explicitly to call them rather than only describe the result in prose. Returns
// "" when there are no tools.
func buildGradeToolGuidance(tools []copilot.Tool) string {
	if len(tools) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("## Recording your result\n\n")
	sb.WriteString("You have been given result-recording tools via MCP. Your decision is captured SOLELY from these tool calls — a result stated only in prose records nothing. When you have finished, you MUST call the appropriate tool(s):\n\n")
	for _, t := range tools {
		fmt.Fprintf(&sb, "- `%s`: %s\n", t.Name, t.Description)
	}
	fmt.Fprintf(&sb, "\nThese tools are exposed under the `%s` MCP server, so their fully-qualified names appear as `mcp__%s__<tool>`. Call them as many times as the task requires.\n", gradeBridgeServerName, gradeBridgeServerName)
	return sb.String()
}
