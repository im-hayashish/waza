package execution

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"

	"github.com/go-viper/mapstructure/v2"
	"github.com/microsoft/waza/internal/models"
)

// This file implements parsing of the Claude Code CLI's `--output-format
// stream-json` output into the pieces an ExecutionResponse needs. It is kept
// free of process spawning so it can be unit-tested against captured fixtures.

// claudeStreamEvent is one line of the stream-json output. The CLI emits a
// sequence of these as newline-delimited JSON. Only the fields we consume are
// declared; unknown fields and unknown event types are ignored so future CLI
// versions don't break parsing.
type claudeStreamEvent struct {
	Type    string         `json:"type"`
	Subtype string         `json:"subtype"`
	Message *claudeMessage `json:"message"`

	// result event fields
	IsError      bool         `json:"is_error"`
	Result       string       `json:"result"`
	NumTurns     int          `json:"num_turns"`
	Usage        *claudeUsage `json:"usage"`
	TotalCostUSD float64      `json:"total_cost_usd"`
	SessionID    string       `json:"session_id"`
}

// claudeMessage is the assistant/user message envelope inside a stream event.
type claudeMessage struct {
	Role    string        `json:"role"`
	Content []claudeBlock `json:"content"`
}

// claudeBlock is one content block. The shape is a union discriminated by Type:
// text | thinking | tool_use | tool_result. Input and Content are RawMessage
// because their shapes vary by tool (Content can be a string or an array of
// sub-blocks).
type claudeBlock struct {
	Type string `json:"type"`

	// text / thinking
	Text string `json:"text"`

	// tool_use
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`

	// tool_result
	ToolUseID string          `json:"tool_use_id"`
	IsError   bool            `json:"is_error"`
	Content   json.RawMessage `json:"content"`
}

// claudeUsage mirrors the usage block on the final result event.
type claudeUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

// claudeStreamResult is the parsed, waza-shaped output of a stream.
type claudeStreamResult struct {
	SessionID        string
	FinalOutput      string
	ToolCalls        []models.ToolCall
	SkillInvocations []SkillInvocation
	Usage            *models.UsageStats
	Success          bool
	ErrorMsg         string

	// toolRecords holds the raw per-tool data (original argument map and
	// tool_result text), in stream order, that buildClaudeTranscript needs to
	// reconstruct the transcript events the copilot engine would emit natively.
	// Kept separate from ToolCalls so that slice stays engine-neutral.
	toolRecords []*claudeToolRecord
}

// claudeToolRecord is the raw record of one tool call used for transcript
// reconstruction: the original argument map plus the tool_result text and
// success flag once the matching result arrives.
type claudeToolRecord struct {
	ID      string
	Name    string
	RawArgs map[string]any
	Success bool
	Result  string
}

const (
	// claudeStreamInitialBuf / claudeStreamMaxBuf size the scanner. Skill-heavy
	// runs can emit very long lines (large tool inputs/results), so the max is
	// generous.
	claudeStreamInitialBuf = 1 << 20 // 1 MiB
	claudeStreamMaxBuf     = 1 << 22 // 4 MiB
)

// decodeToolArgs turns a tool_use input object into models.ToolCallArgs. It uses
// mapstructure (not json.Unmarshal) so that argument keys outside the fixed
// struct fields are captured in ToolCallArgs.Extra (tagged mapstructure:
// ",remain"); the tool_calls / tool_constraint graders read that bag, so without
// this they would see no non-standard arguments.
//
// Claude Code's native file tools name the path argument `file_path` (see
// FileReadInput/FileWriteInput/FileEditInput in the CLI's sdk-tools.d.ts), not
// `path`, so it is aliased onto the canonical Path field the graders index while
// the original key is left in place for Extra. `command`/`description`/`skill`
// already match their struct tags.
func decodeToolArgs(m map[string]any) models.ToolCallArgs {
	var args models.ToolCallArgs
	if len(m) == 0 {
		return args
	}
	if _, ok := m["path"]; !ok {
		if v, ok := m["file_path"].(string); ok {
			m["path"] = v
		}
	}
	_ = mapstructure.Decode(m, &args)
	return args
}

// streamHooks lets Execute observe events as they are parsed from a live stream,
// without coupling the parser to process control. onFirstEvent fires once, when
// the first stream-json line is successfully decoded (the session has started);
// onSkillInvoked fires each time a skill invocation is recorded. Both are optional
// and are called synchronously from the parse loop, so they must not block.
type streamHooks struct {
	onFirstEvent   func()
	onSkillInvoked func()
}

// parseClaudeStream consumes line-delimited stream-json from r and produces a
// claudeStreamResult. It never spawns a process, so it is unit-testable against
// captured fixtures. Malformed lines are skipped rather than failing the whole
// parse; a hard error is only returned if the underlying reader errors. An
// optional streamHooks (at most one) is notified of first-event / skill-invocation
// milestones so callers can enforce FirstEventTimeout / CancelOnSkillInvocation.
func parseClaudeStream(r io.Reader, hooks ...streamHooks) (*claudeStreamResult, error) {
	var h streamHooks
	if len(hooks) > 0 {
		h = hooks[0]
	}
	firstEventSeen := false
	res := &claudeStreamResult{}

	// Tool calls are tracked by tool_use id so we can match a later tool_result
	// to set Success. Maintain insertion order separately for a stable result.
	toolCalls := map[string]*models.ToolCall{}
	var order []string

	// Parallel raw records (for transcript reconstruction), tracked by id and in
	// stream order. No-id tool calls are appended directly.
	recordsByID := map[string]*claudeToolRecord{}
	var recordOrder []*claudeToolRecord

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, claudeStreamInitialBuf), claudeStreamMaxBuf)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var evt claudeStreamEvent
		if err := json.Unmarshal(line, &evt); err != nil {
			// Tolerate non-JSON or partial lines.
			continue
		}

		if !firstEventSeen {
			firstEventSeen = true
			if h.onFirstEvent != nil {
				h.onFirstEvent()
			}
		}

		switch evt.Type {
		case "system":
			if evt.Subtype == "init" && evt.SessionID != "" {
				res.SessionID = evt.SessionID
			}
		case "assistant":
			if evt.Message == nil {
				continue
			}
			for _, b := range evt.Message.Content {
				switch b.Type {
				case "text":
					if b.Text != "" {
						res.FinalOutput = b.Text // last text block wins
					}
				case "tool_use":
					tc := &models.ToolCall{ID: b.ID, Name: b.Name}
					var rawArgs map[string]any
					if len(b.Input) > 0 {
						// Best-effort: decode the raw input map. Ignore errors so
						// tools with unexpected input shapes still record a call.
						_ = json.Unmarshal(b.Input, &rawArgs)
					}
					tc.Arguments = decodeToolArgs(rawArgs)
					rec := &claudeToolRecord{ID: b.ID, Name: b.Name, RawArgs: rawArgs}
					if b.ID != "" {
						if _, exists := toolCalls[b.ID]; !exists {
							order = append(order, b.ID)
							recordOrder = append(recordOrder, rec)
							recordsByID[b.ID] = rec
						}
						toolCalls[b.ID] = tc
					} else {
						// No id to match a result against; record it immediately
						// with optimistic success.
						tc.Success = true
						rec.Success = true
						res.ToolCalls = append(res.ToolCalls, *tc)
						recordOrder = append(recordOrder, rec)
					}
					// Claude Code surfaces a skill invocation as a tool_use named
					// "Skill" whose input carries the skill name under "skill"
					// (e.g. {"skill":"greeting","args":"Bob"}).
					if b.Name == "Skill" && tc.Arguments.Skill != "" {
						res.SkillInvocations = append(res.SkillInvocations, SkillInvocation{Name: tc.Arguments.Skill})
						if h.onSkillInvoked != nil {
							h.onSkillInvoked()
						}
					}
				}
			}
		case "user":
			if evt.Message == nil {
				continue
			}
			for _, b := range evt.Message.Content {
				if b.Type == "tool_result" && b.ToolUseID != "" {
					if tc, ok := toolCalls[b.ToolUseID]; ok {
						tc.Success = !b.IsError
					}
					if rec, ok := recordsByID[b.ToolUseID]; ok {
						rec.Success = !b.IsError
						rec.Result = claudeToolResultText(b.Content)
					}
				}
			}
		case "result":
			res.Success = !evt.IsError
			if evt.Result != "" {
				res.FinalOutput = evt.Result
			}
			if evt.IsError && evt.Subtype != "" {
				res.ErrorMsg = evt.Subtype
			}
			if evt.SessionID != "" && res.SessionID == "" {
				res.SessionID = evt.SessionID
			}
			if evt.Usage != nil {
				res.Usage = &models.UsageStats{
					Turns:            evt.NumTurns,
					InputTokens:      evt.Usage.InputTokens,
					OutputTokens:     evt.Usage.OutputTokens,
					CacheReadTokens:  evt.Usage.CacheReadInputTokens,
					CacheWriteTokens: evt.Usage.CacheCreationInputTokens,
				}
			}
		default:
			// Unknown event types (rate_limit_event, thinking_tokens, ...) are
			// ignored.
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	// Append id-matched tool calls in stream order.
	for _, id := range order {
		res.ToolCalls = append(res.ToolCalls, *toolCalls[id])
	}
	res.toolRecords = recordOrder

	return res, nil
}

// claudeToolResultText extracts the human-readable text from a tool_result
// content field, which the CLI emits either as a bare JSON string or as an array
// of typed blocks (e.g. [{"type":"text","text":"..."}]). Non-text shapes fall
// back to the raw JSON so nothing is silently dropped.
func claudeToolResultText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var sb strings.Builder
		for _, b := range blocks {
			if b.Text != "" {
				if sb.Len() > 0 {
					sb.WriteByte('\n')
				}
				sb.WriteString(b.Text)
			}
		}
		if sb.Len() > 0 {
			return sb.String()
		}
	}
	return string(raw)
}
