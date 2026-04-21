package utils

import (
	"context"
	"log/slog"
	"sync"

	copilot "github.com/github/copilot-sdk/go"
)

// NewSessionToSlog creates a function compatible with [copilot.Session.On] that will
// emit log entries, to slog, when the log level is set to slog.LevelDebug.
func NewSessionToSlog() copilot.SessionEventHandler {
	if !slog.Default().Enabled(context.Background(), slog.LevelDebug) {
		return func(copilot.SessionEvent) {}
	}

	intentCalls := sync.Map{}

	return func(event copilot.SessionEvent) {
		switch event.Type {
		case copilot.PendingMessagesModified, copilot.HookEnd, copilot.HookStart:
			// we just drop these from logging, they're mostly noise, or have other events (like tool calls)
			// that are more informative.
			return
		case copilot.ToolExecutionStart:
			if event.Data.ToolName != nil && *event.Data.ToolName == "report_intent" && event.Data.ToolCallID != nil {
				// store this off, we'll ignore the complete event when it comes in as well.
				intentCalls.Store(*event.Data.ToolCallID, true)
				return
			}
		case copilot.ToolExecutionComplete:
			if event.Data.ToolCallID != nil &&
				intentCalls.CompareAndDelete(*event.Data.ToolCallID, true) {
				return
			}
		}

		sessionToSlog(event)
	}
}

// sessionToSlog tries to be a low-overhead method for dumping out any session events coming from
// the copilot client to slog. It's safe to add this to your copilot session instances, in
// their [copilot.Session.On] handler.
func sessionToSlog(event copilot.SessionEvent) {
	if !slog.Default().Enabled(context.Background(), slog.LevelDebug) {
		return
	}

	attrs := []any{
		"type", event.Type,
	}

	attrs = appendIf(attrs, "reasoningText", event.Data.ReasoningText)

	// session starts
	attrs = appendIf(attrs, "selectedModel", event.Data.SelectedModel)
	attrs = appendIf(attrs, "producer", event.Data.Producer)
	attrs = appendIf(attrs, "sessionID", event.Data.SessionID)

	if event.Data.Context != nil {
		cc := event.Data.Context.ContextClass
		if cc != nil {
			var ccAttrs []any

			ccAttrs = appendIf(ccAttrs, "branch", cc.Branch)
			ccAttrs = append(ccAttrs, "cwd", cc.Cwd)
			ccAttrs = append(ccAttrs, "gitRoot", cc.GitRoot)
			ccAttrs = append(ccAttrs, "repository", cc.Repository)

			attrs = append(attrs, slog.Group("context", ccAttrs...))
		}
	}

	// assistant.turn_start
	attrs = appendIf(attrs, "turnID", event.Data.TurnID)

	// tool calls
	attrs = appendIf(attrs, "content", event.Data.Content)
	attrs = appendIf(attrs, "deltaContent", event.Data.DeltaContent)
	attrs = appendIf(attrs, "toolName", event.Data.ToolName)
	attrs = appendIf(attrs, "toolCallID", event.Data.ToolCallID)

	if event.Data.Result != nil {
		tr := event.Data.Result

		var toolResultArgs []any

		toolResultArgs = appendIf(toolResultArgs, "content", tr.Content)
		toolResultArgs = appendIf(toolResultArgs, "detailedContent", tr.DetailedContent)

		attrs = append(attrs, slog.Group("toolResult", toolResultArgs...))
	}

	// tool call arguments
	attrs = appendMapOfStringAnyIf(attrs, event.Data.Arguments, "arguments")

	// hooks
	attrs = appendIf(attrs, "hookType", event.Data.HookType)
	attrs = appendMapOfStringAnyIf(attrs, event.Data.Input, "input")

	slog.Debug("Event received", attrs...)
}

// appendIf appends the attribute if v is not nil
func appendIf[T any](attrs []any, name string, v *T) []any {
	if v != nil {
		attrs = append(attrs, name)
		attrs = append(attrs, *v)
	}

	return attrs
}

// appendMapOfStringAnyIf appends the contents of the map, as a slog.Group if the
// map is both a map[string]any, and not empty.
// NOTE: the keys are not sorted as they are added to the slog.Group.
func appendMapOfStringAnyIf(attrs []any, mapOfStringAny any, fieldName string) []any {
	if asMap, ok := mapOfStringAny.(map[string]any); ok {
		if len(asMap) == 0 {
			return attrs
		}

		var args []any

		for k, v := range asMap {
			args = append(args, k, v)
		}

		attrs = append(attrs, slog.Group(fieldName, args...))
	}

	return attrs
}
