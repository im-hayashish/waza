package execution

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func readFixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	require.NoError(t, err)
	return string(data)
}

func TestParseClaudeStream_Basic(t *testing.T) {
	res, err := parseClaudeStream(strings.NewReader(readFixture(t, "claude_stream_basic.jsonl")))
	require.NoError(t, err)

	assert.True(t, res.Success)
	assert.Empty(t, res.ErrorMsg)
	assert.Equal(t, "done.", res.FinalOutput)
	assert.NotEmpty(t, res.SessionID)

	require.Len(t, res.ToolCalls, 1)
	assert.Equal(t, "Bash", res.ToolCalls[0].Name)
	assert.Equal(t, "echo hello-from-tool", res.ToolCalls[0].Arguments.Command)
	assert.True(t, res.ToolCalls[0].Success, "tool_result without is_error should map to success")

	require.NotNil(t, res.Usage)
	assert.Equal(t, 18, res.Usage.InputTokens)
	assert.Equal(t, 168, res.Usage.OutputTokens)
	assert.Equal(t, 18074, res.Usage.CacheReadTokens)
	assert.Equal(t, 18226, res.Usage.CacheWriteTokens)
	assert.Equal(t, 2, res.Usage.Turns)

	assert.Empty(t, res.SkillInvocations)
}

func TestParseClaudeStream_SkillInvocation(t *testing.T) {
	res, err := parseClaudeStream(strings.NewReader(readFixture(t, "claude_stream_skill.jsonl")))
	require.NoError(t, err)

	require.Len(t, res.SkillInvocations, 1)
	assert.Equal(t, "greeting-helper", res.SkillInvocations[0].Name)

	// The Skill tool call itself is also recorded, with the skill name in args.
	var found bool
	for _, tc := range res.ToolCalls {
		if tc.Name == "Skill" {
			found = true
			assert.Equal(t, "greeting-helper", tc.Arguments.Skill)
		}
	}
	assert.True(t, found, "expected a Skill tool call in ToolCalls")
}

// TestParseClaudeStream_RealSkillCapture parses a stream captured from a real
// `claude -p --output-format stream-json --verbose` run that invoked a skill,
// so the parser is pinned to the CLI's actual output (including thinking_tokens,
// rate_limit_event, a user text turn, and extra usage fields it must ignore).
func TestParseClaudeStream_RealSkillCapture(t *testing.T) {
	res, err := parseClaudeStream(strings.NewReader(readFixture(t, "claude_stream_real_skill.jsonl")))
	require.NoError(t, err)

	assert.True(t, res.Success)
	assert.Equal(t, "Greetings, Bob! The greeting skill is active.", res.FinalOutput)
	assert.NotEmpty(t, res.SessionID)

	require.Len(t, res.SkillInvocations, 1)
	assert.Equal(t, "greeting", res.SkillInvocations[0].Name)

	var sawSkillTool bool
	for _, tc := range res.ToolCalls {
		if tc.Name == "Skill" {
			sawSkillTool = true
			assert.Equal(t, "greeting", tc.Arguments.Skill)
		}
	}
	assert.True(t, sawSkillTool, "expected the Skill tool call to be recorded")

	require.NotNil(t, res.Usage)
	assert.Positive(t, res.Usage.OutputTokens)
}

// TestParseClaudeStream_ToolArgsExtraAndFilePathAlias verifies that non-standard
// tool arguments are captured into ToolCallArgs.Extra (so argument-matcher
// graders can see them) and that Claude's `file_path` key is aliased onto the
// canonical Path field.
func TestParseClaudeStream_ToolArgsExtraAndFilePathAlias(t *testing.T) {
	const stream = `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"r1","name":"Read","input":{"file_path":"/w/foo.go","limit":50}},{"type":"tool_use","id":"g1","name":"Grep","input":{"pattern":"TODO","glob":"*.go"}}]}}
{"type":"result","subtype":"success","is_error":false}`
	res, err := parseClaudeStream(strings.NewReader(stream))
	require.NoError(t, err)
	require.Len(t, res.ToolCalls, 2)

	read := res.ToolCalls[0]
	assert.Equal(t, "Read", read.Name)
	assert.Equal(t, "/w/foo.go", read.Arguments.Path, "file_path must alias onto Path")
	assert.Equal(t, "/w/foo.go", read.Arguments.Extra["file_path"], "original key stays in Extra")
	assert.EqualValues(t, 50, read.Arguments.Extra["limit"])

	grep := res.ToolCalls[1]
	assert.Equal(t, "Grep", grep.Name)
	assert.Equal(t, "TODO", grep.Arguments.Extra["pattern"], "non-standard args land in Extra for matchers")
	assert.Equal(t, "*.go", grep.Arguments.Extra["glob"])
}

func TestParseClaudeStream_ErrorResult(t *testing.T) {
	const stream = `{"type":"system","subtype":"init","session_id":"sess-err"}
{"type":"result","subtype":"error_max_turns","is_error":true,"num_turns":40,"usage":{"input_tokens":5,"output_tokens":7}}`
	res, err := parseClaudeStream(strings.NewReader(stream))
	require.NoError(t, err)

	assert.False(t, res.Success)
	assert.Equal(t, "error_max_turns", res.ErrorMsg)
	assert.Equal(t, "sess-err", res.SessionID)
	require.NotNil(t, res.Usage)
	assert.Equal(t, 40, res.Usage.Turns)
}

func TestParseClaudeStream_ToolFailureMarksUnsuccessful(t *testing.T) {
	const stream = `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"false"}}]}}
{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","is_error":true,"content":"boom"}]}}
{"type":"result","subtype":"success","is_error":false}`
	res, err := parseClaudeStream(strings.NewReader(stream))
	require.NoError(t, err)

	require.Len(t, res.ToolCalls, 1)
	assert.Equal(t, "Bash", res.ToolCalls[0].Name)
	assert.False(t, res.ToolCalls[0].Success, "is_error=true tool_result should mark the call unsuccessful")
}

func TestParseClaudeStream_ToleratesUnknownAndJunk(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("not json at all\n")
	sb.WriteString(`{"type":"rate_limit_event","foo":"bar"}` + "\n")
	sb.WriteString(`{"type":"system","subtype":"thinking_tokens"}` + "\n")
	sb.WriteString(`{"type":"some_future_event","payload":123}` + "\n")
	sb.WriteString("\n") // blank line
	// An oversized but valid line (still under the 4 MiB cap).
	big := strings.Repeat("x", 2<<20)
	sb.WriteString(`{"type":"assistant","message":{"content":[{"type":"text","text":"` + big + `"}]}}` + "\n")
	sb.WriteString(`{"type":"result","subtype":"success","is_error":false}` + "\n")

	res, err := parseClaudeStream(strings.NewReader(sb.String()))
	require.NoError(t, err)
	assert.True(t, res.Success)
	assert.Len(t, res.FinalOutput, len(big))
}
