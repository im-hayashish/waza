package execution

import (
	copilot "github.com/github/copilot-sdk/go"

	"github.com/microsoft/waza/internal/agentevent"
	"github.com/microsoft/waza/internal/copilotevents"
)

// This file is fork-only. The Claude Code CLI does not hand back the Copilot
// SDK's session-event log, so transcript-based graders (e.g. inline_script,
// which reads `transcript` and derives `tool_calls`) would see nothing. Rather
// than leave that gap, buildClaudeTranscript synthesizes the same event shapes
// the copilot engine emits, using the exported SDK data types that
// internal/models itself already constructs — so no shared code or transcript
// model is forked.
//
// runner.go feeds ExecutionResponse.Events through copilotevents.ToSDK ->
// transcript.BuildFromSessionEvents, which round-trips these back into
// []models.TranscriptEvent and models.FilterToolCalls. Ordering is linear
// (prompt, then tool start/complete pairs, then the final assistant message);
// exact interleaving and timestamps are not recoverable from the stream and are
// not read by graders — TranscriptEvent.MarshalJSON renders only type / content /
// tool-call fields, and FilterToolCalls keys on the tool-call id.
func buildClaudeTranscript(parsed *claudeStreamResult, userMessage string) []agentevent.Event {
	if parsed == nil {
		return nil
	}

	var events []copilot.SessionEvent

	if userMessage != "" {
		events = append(events, copilot.SessionEvent{
			Data: &copilot.UserMessageData{Content: userMessage},
		})
	}

	for _, rec := range parsed.toolRecords {
		if rec == nil {
			continue
		}
		events = append(events,
			copilot.SessionEvent{
				Data: &copilot.ToolExecutionStartData{
					ToolName:   rec.Name,
					ToolCallID: rec.ID,
					Arguments:  rec.RawArgs,
				},
			},
			copilot.SessionEvent{
				Data: &copilot.ToolExecutionCompleteData{
					ToolCallID: rec.ID,
					Success:    rec.Success,
					Result:     &copilot.ToolExecutionCompleteResult{Content: rec.Result},
				},
			},
		)
	}

	if parsed.FinalOutput != "" {
		events = append(events, copilot.SessionEvent{
			Data: &copilot.AssistantMessageData{Content: parsed.FinalOutput},
		})
	}

	return copilotevents.FromSDK(events)
}
