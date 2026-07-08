package execution

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	copilot "github.com/github/copilot-sdk/go"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/microsoft/waza/internal/copilotevents"
	"github.com/microsoft/waza/internal/models"
	"github.com/microsoft/waza/internal/transcript"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBuildClaudeTranscript_RoundTrip verifies the synthesized transcript events
// survive the exact path the runner uses to feed transcript-based graders
// (copilotevents.ToSDK -> models.FilterToolCalls / transcript.BuildFromSessionEvents),
// yielding the tool calls (name, args, success, result) and the user/assistant
// content an inline_script grader reads.
func TestBuildClaudeTranscript_RoundTrip(t *testing.T) {
	const stream = `{"type":"system","subtype":"init","session_id":"s1"}
{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"echo hi"}}]}}
{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","is_error":false,"content":"hi"}]}}
{"type":"assistant","message":{"content":[{"type":"text","text":"done"}]}}
{"type":"result","subtype":"success","is_error":false,"result":"done"}`
	parsed, err := parseClaudeStream(strings.NewReader(stream))
	require.NoError(t, err)

	events := buildClaudeTranscript(parsed, "run echo")
	require.NotEmpty(t, events, "claude runs must populate the transcript so inline_script graders see data")

	sdk := copilotevents.ToSDK(events)

	toolCalls := models.FilterToolCalls(sdk)
	require.Len(t, toolCalls, 1)
	assert.Equal(t, "Bash", toolCalls[0].Name)
	assert.Equal(t, "echo hi", toolCalls[0].Arguments.Command)
	assert.True(t, toolCalls[0].Success)
	require.NotNil(t, toolCalls[0].Result)
	assert.Equal(t, "hi", toolCalls[0].Result.Content)

	var joined string
	for _, e := range transcript.BuildFromSessionEvents(sdk) {
		b, err := e.MarshalJSON()
		require.NoError(t, err)
		joined += string(b)
	}
	assert.Contains(t, joined, "run echo", "user prompt should be in the transcript")
	assert.Contains(t, joined, "done", "assistant final output should be in the transcript")
	assert.Contains(t, joined, "echo hi", "tool arguments should be in the transcript")
}

// gradeToolPair builds a set_waza_grade_pass/fail tool pair whose handlers append
// the reason to the given slices, mirroring the real prompt grader's tools. Used
// by the live bridge E2E test.
func gradeToolPair(passes, failures *[]string) []copilot.Tool {
	mk := func(name string, sink *[]string) copilot.Tool {
		return copilot.Tool{
			Name:        name,
			Description: "Used by waza graders to record a verdict. Can be called multiple times.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"reason": map[string]any{"type": "string", "description": "why"},
				},
			},
			Handler: func(inv copilot.ToolInvocation) (copilot.ToolResult, error) {
				m, _ := inv.Arguments.(map[string]any)
				*sink = append(*sink, fmt.Sprint(m["reason"]))
				return copilot.ToolResult{}, nil
			},
		}
	}
	return []copilot.Tool{mk("set_waza_grade_pass", passes), mk("set_waza_grade_fail", failures)}
}

// TestClaudeCodeEngine_PromptGraderBridge_E2E drives a real `claude` judge run
// through the engine with bridged grade tools and asserts the judge's tool call
// reached the in-process handler (populating the pass slice). Gated behind
// WAZA_CLAUDE_E2E so ordinary `go test` never spends tokens.
func TestClaudeCodeEngine_PromptGraderBridge_E2E(t *testing.T) {
	if os.Getenv("WAZA_CLAUDE_E2E") == "" {
		t.Skip("set WAZA_CLAUDE_E2E=1 to run the live claude-code prompt-grader bridge test")
	}
	const model = "claude-haiku-4-5-20251001"
	e := NewClaudeCodeEngine(model)
	require.NoError(t, e.Initialize(context.Background()))
	t.Cleanup(func() { _ = e.Shutdown(context.Background()) })

	var passes, failures []string
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	_, err := e.Execute(ctx, &ExecutionRequest{
		ModelID:              model,
		Message:              "You are grading a candidate answer to the question 'What is 2+2?'. The candidate answered '4'. Decide whether it is correct and record your verdict using the grade tools.",
		Tools:                gradeToolPair(&passes, &failures),
		EphemeralSession:     true,
		SkipWorkspaceCapture: true,
		NoSkills:             true,
	})
	require.NoError(t, err)
	require.NotEmpty(t, passes, "judge should record a passing grade via the bridged MCP tool (passes=%v failures=%v)", passes, failures)
	require.Empty(t, failures, "a correct answer should not record a failure (failures=%v)", failures)
}

func TestClaudeCredentialsPresent(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	assert.False(t, claudeCredentialsPresent(), "no credentials file yet")

	require.NoError(t, os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte("{}"), 0o600))
	assert.True(t, claudeCredentialsPresent(), "credentials file should be detected")
}

func TestClaudeCodeEngine_InitializeAuth(t *testing.T) {
	if _, err := exec.LookPath("claude"); err != nil {
		if _, statErr := os.Stat("/usr/bin/claude"); statErr != nil {
			t.Skip("claude CLI not available")
		}
	}

	// No env token and an empty config dir => no auth => clear error.
	t.Setenv(oauthTokenEnv, "")
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	err := NewClaudeCodeEngine("haiku").Initialize(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), oauthTokenEnv)

	// Stored credentials (from `claude setup-token`) satisfy auth even when the
	// env token is unset.
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte("{}"), 0o600))
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	assert.NoError(t, NewClaudeCodeEngine("haiku").Initialize(context.Background()))
}

func TestPurgeWorkspaceProjects(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)

	// Simulate the CLI's layout for two of our workspaces: one persisted (has a
	// session .jsonl), and one ephemeral judge run (memory/ subdir only, no
	// session file — the case the old session-ID purge missed). Plus a WorkDir
	// subdirectory variant, which the encoded name carries as a trailing segment.
	persisted := filepath.Join(cfg, "projects", "-tmp-waza-claude-999")
	require.NoError(t, os.MkdirAll(persisted, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(persisted, "sid.jsonl"), []byte("{}"), 0o644))

	ephemeral := filepath.Join(cfg, "projects", "-tmp-waza-claude-777-sub")
	require.NoError(t, os.MkdirAll(filepath.Join(ephemeral, "memory"), 0o755))

	// An unrelated project dir must survive.
	other := filepath.Join(cfg, "projects", "-other")
	require.NoError(t, os.MkdirAll(other, 0o755))

	purgeWorkspaceProjects([]string{"/tmp/waza-claude-999", "/tmp/waza-claude-777", ""})

	_, err := os.Stat(persisted)
	assert.True(t, os.IsNotExist(err), "a persisted workspace project dir should be removed")
	_, err = os.Stat(ephemeral)
	assert.True(t, os.IsNotExist(err), "an ephemeral (memory-only) workspace project dir should be removed")
	_, err = os.Stat(other)
	assert.NoError(t, err, "an unrelated project dir must be preserved")
}

func TestClaudeCodeEngine_ExecuteBeforeInitialize(t *testing.T) {
	e := NewClaudeCodeEngine("haiku")
	_, err := e.Execute(context.Background(), &ExecutionRequest{Message: "hi"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not initialized")
}

// TestGradeToolBridge_RoutesCallToHandler verifies the in-process MCP bridge
// forwards a tool call straight to the originating copilot.Tool.Handler closure
// (the mechanism the prompt/LLM-judge grader relies on to collect Passes/Failures).
func TestGradeToolBridge_RoutesCallToHandler(t *testing.T) {
	var gotArgs map[string]any
	tool := copilot.Tool{
		Name:        "set_waza_grade_pass",
		Description: "mark pass",
		Parameters:  map[string]any{"type": "object"},
		Handler: func(inv copilot.ToolInvocation) (copilot.ToolResult, error) {
			m, _ := inv.Arguments.(map[string]any)
			gotArgs = m
			return copilot.ToolResult{TextResultForLLM: "ok"}, nil
		},
	}

	handler := newBridgeHandler(tool)
	var req mcp.CallToolRequest
	req.Params.Name = "set_waza_grade_pass"
	req.Params.Arguments = map[string]any{"reason": "looks good"}

	res, err := handler(context.Background(), req)
	require.NoError(t, err)
	require.NotNil(t, res)
	require.Equal(t, "looks good", gotArgs["reason"], "MCP call args must reach the copilot handler")
}

// TestGradeToolBridge_StartServesAndShutsDown ensures the bridge stands up an
// HTTP listener with a usable URL and that shutdown is clean and idempotent.
func TestGradeToolBridge_StartServesAndShutsDown(t *testing.T) {
	b, err := startGradeToolBridge([]copilot.Tool{{
		Name:       "set_waza_grade_pass",
		Parameters: map[string]any{"type": "object"},
		Handler:    func(copilot.ToolInvocation) (copilot.ToolResult, error) { return copilot.ToolResult{}, nil },
	}})
	require.NoError(t, err)
	require.Contains(t, b.url, "http://127.0.0.1:")
	require.Contains(t, b.url, "/mcp")
	b.shutdown()
	b.shutdown() // idempotent
}

// TestBuildGradeToolGuidance verifies the judge guidance names each tool and the
// MCP namespace so the judge knows what to call.
func TestBuildGradeToolGuidance(t *testing.T) {
	assert.Empty(t, buildGradeToolGuidance(nil))

	g := buildGradeToolGuidance([]copilot.Tool{
		{Name: "set_waza_grade_pass", Description: "mark pass"},
		{Name: "set_waza_grade_fail", Description: "mark fail"},
	})
	assert.Contains(t, g, "set_waza_grade_pass")
	assert.Contains(t, g, "set_waza_grade_fail")
	assert.Contains(t, g, "mcp__"+gradeBridgeServerName+"__")
}

// TestBuildMCPConfig_IncludesGradeBridge verifies the grade-tool bridge is added
// as an HTTP MCP server under gradeBridgeServerName when a bridge URL is present.
func TestBuildMCPConfig_IncludesGradeBridge(t *testing.T) {
	data, err := buildMCPConfig(nil, "http://127.0.0.1:12345/mcp")
	require.NoError(t, err)
	require.NotNil(t, data)

	var parsed struct {
		MCPServers map[string]map[string]any `json:"mcpServers"`
	}
	require.NoError(t, json.Unmarshal(data, &parsed))
	bridge := parsed.MCPServers[gradeBridgeServerName]
	require.NotNil(t, bridge, "grade bridge must be present in the MCP config")
	assert.Equal(t, "http", bridge["type"])
	assert.Equal(t, "http://127.0.0.1:12345/mcp", bridge["url"])
}

// TestBuildClaudeArgs_EphemeralSessionNotPersisted verifies a fresh ephemeral
// session (the judge run) passes --no-session-persistence, while a resumed one
// does not (so --resume keeps working).
func TestBuildClaudeArgs_EphemeralSessionNotPersisted(t *testing.T) {
	fresh := buildClaudeArgs(&ExecutionRequest{Message: "judge", EphemeralSession: true}, "haiku", "/tmp/ws", "", "")
	assert.Contains(t, fresh, "--no-session-persistence")

	resumed := buildClaudeArgs(&ExecutionRequest{Message: "judge", EphemeralSession: true, SessionID: "s1"}, "haiku", "/tmp/ws", "", "")
	assert.NotContains(t, resumed, "--no-session-persistence")
	assert.Contains(t, resumed, "--resume")
}

// writeFakeClaude writes an executable shell script that stands in for the
// `claude` binary, emitting the given script body, and returns its path. Used to
// drive the engine's subprocess handling deterministically without a real CLI.
func writeFakeClaude(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-claude.sh")
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755))
	return path
}

// TestExecute_CancelOnSkillInvocation verifies the engine terminates the turn as
// soon as the awaited skill fires (rather than waiting for the full turn) and
// reports that early exit as success with the skill recorded.
func TestExecute_CancelOnSkillInvocation(t *testing.T) {
	// Emits an init event and a Skill tool_use, then blocks — the engine must
	// kill it early instead of waiting out the (long) sleep.
	script := writeFakeClaude(t, `echo '{"type":"system","subtype":"init","session_id":"sess-skill"}'
echo '{"type":"assistant","message":{"content":[{"type":"tool_use","id":"s1","name":"Skill","input":{"skill":"greeting"}}]}}'
exec sleep 30
`)
	e := NewClaudeCodeEngine("haiku")
	e.binPath = script
	e.initCalled.Store(true)
	t.Cleanup(func() { _ = e.Shutdown(context.Background()) })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := time.Now()
	resp, err := e.Execute(ctx, &ExecutionRequest{
		Message:                 "go",
		WorkspaceDir:            t.TempDir(),
		NoSkills:                true,
		SkipWorkspaceCapture:    true,
		CancelOnSkillInvocation: true,
	})
	require.NoError(t, err)
	assert.Less(t, time.Since(start), 5*time.Second, "must cancel early, not wait for the sleep")
	assert.True(t, resp.Success, "early skill-cancel is expected, not a failure")
	assert.Empty(t, resp.ErrorMsg)
	require.Len(t, resp.SkillInvocations, 1)
	assert.Equal(t, "greeting", resp.SkillInvocations[0].Name)
}

// TestExecute_FirstEventTimeout verifies a session-start hang (no stream output)
// is aborted with the distinct first-event-timeout error rather than blocking
// until the overall context deadline.
func TestExecute_FirstEventTimeout(t *testing.T) {
	script := writeFakeClaude(t, "exec sleep 30\n") // never emits an event
	e := NewClaudeCodeEngine("haiku")
	e.binPath = script
	e.initCalled.Store(true)
	t.Cleanup(func() { _ = e.Shutdown(context.Background()) })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := time.Now()
	_, err := e.Execute(ctx, &ExecutionRequest{
		Message:              "go",
		WorkspaceDir:         t.TempDir(),
		NoSkills:             true,
		SkipWorkspaceCapture: true,
		FirstEventTimeout:    300 * time.Millisecond,
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, errClaudeFirstEventTimeout)
	assert.Less(t, time.Since(start), 5*time.Second, "must abort on the first-event budget, not the ctx deadline")
}

// TestExecute_FirstEventTimeoutDisarmed verifies the watchdog does not fire when
// the first event arrives promptly, even with a short budget: a normal turn
// completes successfully.
func TestExecute_FirstEventTimeoutDisarmed(t *testing.T) {
	script := writeFakeClaude(t, `echo '{"type":"system","subtype":"init","session_id":"sess-ok"}'
echo '{"type":"assistant","message":{"content":[{"type":"text","text":"hi"}]}}'
echo '{"type":"result","subtype":"success","is_error":false,"result":"hi"}'
`)
	e := NewClaudeCodeEngine("haiku")
	e.binPath = script
	e.initCalled.Store(true)
	t.Cleanup(func() { _ = e.Shutdown(context.Background()) })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := e.Execute(ctx, &ExecutionRequest{
		Message:              "go",
		WorkspaceDir:         t.TempDir(),
		NoSkills:             true,
		SkipWorkspaceCapture: true,
		FirstEventTimeout:    300 * time.Millisecond,
	})
	require.NoError(t, err)
	assert.True(t, resp.Success)
	assert.Equal(t, "hi", resp.FinalOutput)
}

// TestClaudeCodeEngine_TranscriptPopulated_E2E drives a real `claude` run that
// uses a tool and asserts the transcript (ExecutionResponse.Events) is populated
// so transcript-based graders see the tool call. Gated behind WAZA_CLAUDE_E2E.
func TestClaudeCodeEngine_TranscriptPopulated_E2E(t *testing.T) {
	if os.Getenv("WAZA_CLAUDE_E2E") == "" {
		t.Skip("set WAZA_CLAUDE_E2E=1 to run the live claude-code transcript test")
	}
	const model = "claude-haiku-4-5-20251001"
	e := NewClaudeCodeEngine(model)
	require.NoError(t, e.Initialize(context.Background()))
	t.Cleanup(func() { _ = e.Shutdown(context.Background()) })

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	resp, err := e.Execute(ctx, &ExecutionRequest{
		ModelID:              model,
		Message:              "Run exactly this shell command with the Bash tool: echo WAZA_TRANSCRIPT_OK. Then reply 'done'.",
		WorkspaceDir:         t.TempDir(),
		NoSkills:             true,
		SkipWorkspaceCapture: true,
	})
	require.NoError(t, err)
	require.NotEmpty(t, resp.Events, "a real run must populate the transcript")

	toolCalls := models.FilterToolCalls(copilotevents.ToSDK(resp.Events))
	require.NotEmpty(t, toolCalls, "the Bash tool call must appear in the transcript")
	var sawBash bool
	for _, tc := range toolCalls {
		if tc.Name == "Bash" {
			sawBash = true
		}
	}
	assert.True(t, sawBash, "expected a Bash tool call in the transcript; got %+v", toolCalls)
}

func TestBuildClaudeArgs_AppendsInstructionsFile(t *testing.T) {
	req := &ExecutionRequest{Message: "hi"}
	args := buildClaudeArgs(req, "haiku", "/tmp/ws", "", "/tmp/sys.txt")

	found := false
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "--append-system-prompt-file" && args[i+1] == "/tmp/sys.txt" {
			found = true
		}
	}
	assert.True(t, found, "instruction files must be passed via --append-system-prompt-file")

	// No system-prompt path -> no flag (and never the inline variant, which would
	// risk exceeding MAX_ARG_STRLEN on large instruction sets).
	bare := buildClaudeArgs(req, "haiku", "/tmp/ws", "", "")
	assert.NotContains(t, bare, "--append-system-prompt-file")
	assert.NotContains(t, bare, "--append-system-prompt")
}

// TestWriteSystemPromptFile verifies the instruction files are rendered to a
// temp file whose contents carry the instruction text, and that an empty
// instruction set yields no file.
func TestWriteSystemPromptFile(t *testing.T) {
	empty, err := writeSystemPromptFile(nil, nil)
	require.NoError(t, err)
	assert.Empty(t, empty, "no instructions -> no file")

	path, err := writeSystemPromptFile([]InstructionFile{{Path: "proj.md", Content: []byte("always be brief")}}, nil)
	require.NoError(t, err)
	require.NotEmpty(t, path)
	defer os.Remove(path)

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(data), "always be brief")
}

func TestBuildClaudeArgs(t *testing.T) {
	req := &ExecutionRequest{Message: "do the thing"}
	args := buildClaudeArgs(req, "haiku", "/tmp/ws", "", "")

	// Prompt is passed via -p.
	assert.Equal(t, "-p", args[0])
	assert.Equal(t, "do the thing", args[1])

	joined := args
	assertContainsPair := func(flag, val string) {
		t.Helper()
		for i := 0; i < len(joined)-1; i++ {
			if joined[i] == flag && joined[i+1] == val {
				return
			}
		}
		t.Fatalf("expected flag %q with value %q in %v", flag, val, joined)
	}
	assertContainsPair("--output-format", "stream-json")
	assertContainsPair("--permission-mode", "bypassPermissions")
	assertContainsPair("--add-dir", "/tmp/ws")
	assertContainsPair("--model", "haiku")
	assert.Contains(t, joined, "--verbose")

	// First turn (no SessionID) must not resume, and must not disable session
	// persistence — the session has to be saved so a follow-up can resume it.
	assert.NotContains(t, joined, "--resume")
	assert.NotContains(t, joined, "--no-session-persistence")
}

func TestBuildClaudeArgs_ResumesSessionOnFollowUp(t *testing.T) {
	req := &ExecutionRequest{Message: "and now greet Alice", SessionID: "sess-abc-123"}
	args := buildClaudeArgs(req, "haiku", "/tmp/ws", "", "")

	found := false
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "--resume" && args[i+1] == "sess-abc-123" {
			found = true
		}
	}
	assert.True(t, found, "a request carrying a SessionID must resume that conversation")
}

func TestBuildMCPConfig(t *testing.T) {
	data, err := buildMCPConfig(nil, "")
	require.NoError(t, err)
	assert.Nil(t, data, "no servers -> nil config")

	servers := map[string]copilot.MCPServerConfig{
		"local":  copilot.MCPStdioServerConfig{Command: "node", Args: []string{"s.js"}, Env: map[string]string{"K": "v"}},
		"remote": copilot.MCPHTTPServerConfig{URL: "https://mcp.example.com", Headers: map[string]string{"Authorization": "Bearer x"}},
	}
	data, err = buildMCPConfig(servers, "")
	require.NoError(t, err)
	require.NotNil(t, data)

	var parsed struct {
		MCPServers map[string]map[string]any `json:"mcpServers"`
	}
	require.NoError(t, json.Unmarshal(data, &parsed))
	require.Len(t, parsed.MCPServers, 2)

	local := parsed.MCPServers["local"]
	assert.Equal(t, "stdio", local["type"])
	assert.Equal(t, "node", local["command"])
	assert.Equal(t, []any{"s.js"}, local["args"])

	remote := parsed.MCPServers["remote"]
	assert.Equal(t, "http", remote["type"])
	assert.Equal(t, "https://mcp.example.com", remote["url"])
}

func TestNormalizeClaudeModel(t *testing.T) {
	cases := map[string]string{
		// Copilot dotted names → hyphenated CLI IDs
		"claude-haiku-4.5":  "claude-haiku-4-5",
		"claude-opus-4.6":   "claude-opus-4-6",
		"claude-sonnet-4.6": "claude-sonnet-4-6",
		"claude-opus-4.8":   "claude-opus-4-8",
		// Already-valid / passthrough values are untouched
		"haiku":                     "haiku",
		"claude-haiku-4-5":          "claude-haiku-4-5",
		"claude-haiku-4-5-20251001": "claude-haiku-4-5-20251001",
		"gpt-5-mini":                "gpt-5-mini",
		"":                          "",
		// Only the final minor-version dot is rewritten, nothing mid-string
		"claude-sonnet-4.6-something": "claude-sonnet-4.6-something",
	}
	for in, want := range cases {
		if got := normalizeClaudeModel(in); got != want {
			t.Errorf("normalizeClaudeModel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBuildClaudeArgs_OmitsModelWhenEmpty(t *testing.T) {
	args := buildClaudeArgs(&ExecutionRequest{Message: "x"}, "", "/tmp/ws", "", "")
	assert.NotContains(t, args, "--model")
}

func TestFilteredEnv_DropsAPIKeyKeepsOAuth(t *testing.T) {
	in := []string{
		"PATH=/usr/bin",
		apiKeyEnv + "=sk-secret",
		oauthTokenEnv + "=oauth-token",
		"HOME=/home/x",
	}
	out := filteredEnv(in)

	for _, kv := range out {
		assert.NotEqual(t, apiKeyEnv+"=sk-secret", kv, "ANTHROPIC_API_KEY must be dropped")
	}
	assert.Contains(t, out, oauthTokenEnv+"=oauth-token", "OAuth token must be preserved")
	assert.Contains(t, out, "PATH=/usr/bin")
}

func TestMaterializeSkills_LinksSkillIntoWorkspace(t *testing.T) {
	// A skill source dir laid out as <root>/<skill>/SKILL.md plus a reference.
	root := t.TempDir()
	skillDir := filepath.Join(root, "my-skill")
	require.NoError(t, os.MkdirAll(filepath.Join(skillDir, "references"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(skillDir, "SKILL.md"),
		[]byte("---\nname: my-skill\ndescription: test skill\n---\nbody\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(skillDir, "references", "x.md"),
		[]byte("reference content"), 0o644))

	ws := t.TempDir()
	req := &ExecutionRequest{SkillPaths: []string{root}}
	require.NoError(t, materializeSkills(ws, req))

	// SKILL.md is reachable through the linked path.
	linkedSkill := filepath.Join(ws, ".claude", "skills", "my-skill", "SKILL.md")
	data, err := os.ReadFile(linkedSkill)
	require.NoError(t, err)
	assert.Contains(t, string(data), "name: my-skill")

	// references/ resolve through the link too.
	ref, err := os.ReadFile(filepath.Join(ws, ".claude", "skills", "my-skill", "references", "x.md"))
	require.NoError(t, err)
	assert.Equal(t, "reference content", string(ref))
}

// TestMaterializeSkills_RejectsUnsafeSkillName verifies that a skill whose
// SKILL.md declares a path-traversal `name:` is skipped and cannot be
// materialized outside <workspace>/.claude/skills.
func TestMaterializeSkills_RejectsUnsafeSkillName(t *testing.T) {
	root := t.TempDir()
	skillDir := filepath.Join(root, "sneaky")
	require.NoError(t, os.MkdirAll(skillDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(skillDir, "SKILL.md"),
		[]byte("---\nname: ../../escaped\ndescription: evil\n---\nbody\n"), 0o644))

	ws := t.TempDir()
	req := &ExecutionRequest{SkillPaths: []string{root}}
	require.NoError(t, materializeSkills(ws, req))

	// The traversal target (<ws>/escaped, where "../../escaped" resolves from
	// the skills root) must not have been created.
	_, err := os.Lstat(filepath.Join(ws, "escaped"))
	assert.True(t, os.IsNotExist(err), "unsafe skill name must not escape the skills root")
}

// TestMaterializeSkills_FallbackViaSourceDir verifies that when skill_directories
// is not configured (SkillPaths empty) but SourceDir and SkillName are set, the
// skill is found by walking up from SourceDir and materialized in the workspace.
// This covers the .claude/skills/<name>/ layout where the eval lives inside
// aidd-eval/evals/demand-analysis/ and the target skill is a sibling of aidd-eval.
func TestMaterializeSkills_FallbackViaSourceDir(t *testing.T) {
	// Build a directory tree that mirrors the real layout:
	//   <root>/
	//     my-skill/SKILL.md      ← sibling skill
	//     my-skill/references/x.md
	//     aidd-eval/evals/spec/  ← specDir (SourceDir)
	root := t.TempDir()
	skillDir := filepath.Join(root, "my-skill")
	require.NoError(t, os.MkdirAll(filepath.Join(skillDir, "references"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(skillDir, "SKILL.md"),
		[]byte("---\nname: my-skill\ndescription: fallback skill\n---\nbody\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(skillDir, "references", "rule.md"),
		[]byte("rule content"), 0o644))

	specDir := filepath.Join(root, "aidd-eval", "evals", "spec")
	require.NoError(t, os.MkdirAll(specDir, 0o755))

	ws := t.TempDir()
	// No SkillPaths set — simulates missing skill_directories in eval.yaml.
	req := &ExecutionRequest{
		SkillName: "my-skill",
		SourceDir: specDir,
	}
	require.NoError(t, materializeSkills(ws, req))

	// Skill must be reachable via .claude/skills even without skill_directories.
	skillMD := filepath.Join(ws, ".claude", "skills", "my-skill", "SKILL.md")
	data, err := os.ReadFile(skillMD)
	require.NoError(t, err)
	assert.Contains(t, string(data), "name: my-skill")

	ref, err := os.ReadFile(filepath.Join(ws, ".claude", "skills", "my-skill", "references", "rule.md"))
	require.NoError(t, err)
	assert.Equal(t, "rule content", string(ref))
}

func TestFindSkillDirByName(t *testing.T) {
	// Tree:  <root>/skills/target-skill/SKILL.md
	//        <root>/skills/aidd-eval/evals/spec/  ← baseDir
	root := t.TempDir()
	skillsDir := filepath.Join(root, "skills")
	targetDir := filepath.Join(skillsDir, "target-skill")
	require.NoError(t, os.MkdirAll(targetDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(targetDir, "SKILL.md"), []byte("---\nname: target-skill\n---"), 0o644))

	baseDir := filepath.Join(root, "skills", "aidd-eval", "evals", "spec")
	require.NoError(t, os.MkdirAll(baseDir, 0o755))

	got := findSkillDirByName("target-skill", baseDir)
	assert.Equal(t, skillsDir, got, "should find the parent dir containing target-skill/")
}

func TestFindSkillDirByName_NotFound(t *testing.T) {
	root := t.TempDir()
	got := findSkillDirByName("nonexistent-skill", root)
	assert.Equal(t, "", got)
}

func TestCaptureWorkspaceFilesExcludingClaude(t *testing.T) {
	ws := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(ws, "out.txt"), []byte("hello"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(ws, ".claude", "skills"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(ws, ".claude", "settings.json"), []byte("{}"), 0o644))

	files := captureWorkspaceFilesExcludingClaude(ws)
	assert.Contains(t, files, "out.txt")
	for k := range files {
		assert.False(t, k == ".claude" || strings.HasPrefix(k, ".claude/"), "should exclude .claude: %s", k)
	}
}
