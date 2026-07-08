package execution

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	copilot "github.com/github/copilot-sdk/go"
	"github.com/microsoft/waza/internal/models"
)

// ClaudeCodeEngine drives the Claude Code CLI (`claude`) headlessly so that
// evaluations run against a contracted Claude subscription instead of the
// GitHub Copilot premium-request quota. It implements AgentEngine and
// WorkspaceKeeper, mirroring MockEngine's workspace lifecycle while delegating
// the actual agent turn to a `claude -p --output-format stream-json`
// subprocess. The stream-json output is mapped to ExecutionResponse by
// parseClaudeStream (claude_stream.go).
type ClaudeCodeEngine struct {
	defaultModelID string
	binPath        string

	keepWorkspace bool

	mu           sync.Mutex
	workspaces   []string
	gitResources []GitResource
	usage        map[string]*models.UsageStats

	initCalled atomic.Bool
}

// oauthTokenEnv is the environment variable carrying the subscription OAuth
// token issued by `claude setup-token`.
const oauthTokenEnv = "CLAUDE_CODE_OAUTH_TOKEN"

// apiKeyEnv is unset for the child process so the CLI does not fall back to
// metered API billing instead of the subscription seat.
const apiKeyEnv = "ANTHROPIC_API_KEY"

// errClaudeFirstEventTimeout and errClaudeCanceledForSkill are run-context
// cancellation causes used to distinguish the engine's own aborts — a
// session-start hang (FirstEventTimeout) and CancelOnSkillInvocation early
// termination — from a caller cancellation or normal completion.
var (
	errClaudeFirstEventTimeout = errors.New("no first stream event within first-event timeout")
	errClaudeCanceledForSkill  = errors.New("canceled after skill invocation")
)

// Compile-time assertions that ClaudeCodeEngine satisfies the engine contracts.
var (
	_ AgentEngine     = (*ClaudeCodeEngine)(nil)
	_ WorkspaceKeeper = (*ClaudeCodeEngine)(nil)
)

// NewClaudeCodeEngine creates a new Claude Code engine with the given default
// model ID (used when an ExecutionRequest does not specify one).
func NewClaudeCodeEngine(modelID string) *ClaudeCodeEngine {
	return &ClaudeCodeEngine{
		defaultModelID: modelID,
		usage:          map[string]*models.UsageStats{},
	}
}

// SetKeepWorkspace enables or disables workspace preservation on shutdown.
func (e *ClaudeCodeEngine) SetKeepWorkspace(keep bool) {
	e.keepWorkspace = keep
}

// Initialize locates the `claude` binary and verifies subscription auth is
// available. It does not start any long-lived process; each Execute spawns its
// own CLI invocation.
func (e *ClaudeCodeEngine) Initialize(ctx context.Context) error {
	bin, err := exec.LookPath("claude")
	if err != nil {
		// Fall back to the well-known devcontainer install location.
		const fallback = "/usr/bin/claude"
		if _, statErr := os.Stat(fallback); statErr == nil {
			bin = fallback
		} else {
			return fmt.Errorf("claude CLI not found on PATH: %w", err)
		}
	}
	e.binPath = bin

	// Auth may come either from CLAUDE_CODE_OAUTH_TOKEN in the environment or
	// from credentials the CLI stored via `claude setup-token`. Only fail early
	// when neither is available; otherwise let the CLI use whichever it has.
	if os.Getenv(oauthTokenEnv) == "" && !claudeCredentialsPresent() {
		return fmt.Errorf("no Claude subscription auth found: set %s or run `claude setup-token`", oauthTokenEnv)
	}

	e.initCalled.Store(true)
	return nil
}

// claudeConfigDir returns the Claude CLI's config directory, honoring
// CLAUDE_CONFIG_DIR and falling back to ~/.claude. Returns "" if the home
// directory can't be resolved and no override is set.
func claudeConfigDir() string {
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude")
}

// claudeCredentialsPresent reports whether the Claude CLI has stored
// subscription credentials (written by `claude setup-token`), which it uses to
// authenticate when CLAUDE_CODE_OAUTH_TOKEN is not set in the environment.
func claudeCredentialsPresent() bool {
	dir := claudeConfigDir()
	if dir == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(dir, ".credentials.json"))
	return err == nil
}

// purgeWorkspaceProjects removes the per-project directories the CLI created
// under <config>/projects for each of our temp workspaces. The CLI derives that
// directory from the run's cwd by replacing path separators, so the workspace's
// (globally unique) basename appears verbatim in the encoded name; globbing on it
// removes the whole directory — session .jsonl transcripts and the auto-memory/
// subdir alike. This is keyed on the workspaces we own (not session IDs) so it
// also cleans ephemeral judge runs, which pass --no-session-persistence and so
// leave a memory-only directory behind with no session file to match. Best-effort:
// failures are logged, not returned, so they never fail Shutdown.
func purgeWorkspaceProjects(workspaces []string) {
	cfg := claudeConfigDir()
	if cfg == "" {
		return
	}
	projects := filepath.Join(cfg, "projects")
	for _, ws := range workspaces {
		base := filepath.Base(ws)
		if base == "" || base == "." || base == string(filepath.Separator) {
			continue
		}
		// Our workspace basenames ("waza-claude-<random>") contain no glob
		// metacharacters, so this pattern is safe. Wildcards on both sides match
		// both the workspace dir and any WorkDir subdirectory the CLI ran in.
		matches, err := filepath.Glob(filepath.Join(projects, "*"+base+"*"))
		if err != nil {
			continue
		}
		for _, m := range matches {
			if err := os.RemoveAll(m); err != nil {
				slog.Warn("failed to purge claude project dir", "path", m, "error", err)
			}
		}
	}
}

// Execute runs one agent turn for req by spawning the Claude CLI in a
// per-task workspace and mapping its stream-json output to ExecutionResponse.
func (e *ClaudeCodeEngine) Execute(ctx context.Context, req *ExecutionRequest) (*ExecutionResponse, error) {
	if !e.initCalled.Load() {
		return nil, fmt.Errorf("engine was not initialized. Initialize needs to be called before Execute")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// The Claude Code CLI is a subprocess: it cannot invoke in-process Go tool
	// handlers directly. Requests that carry such handlers come from the
	// prompt/LLM-judge grader (set_waza_grade_* / set_pairwise_winner) and the
	// responder loop (responder_reply/stop/abstain). Bridge those handlers to the
	// CLI over an in-process MCP server so the model can call them and the caller's
	// captured state (grader Passes/Failures, responder Decision) populates normally.
	var gradeBridge *gradeToolBridge
	if len(req.Tools) > 0 {
		b, err := startGradeToolBridge(req.Tools)
		if err != nil {
			return nil, fmt.Errorf("failed to start grade-tool bridge for prompt/LLM-judge grader: %w", err)
		}
		gradeBridge = b
		defer gradeBridge.shutdown()
	}

	start := time.Now()

	workspaceDir, err := e.resolveWorkspace(ctx, req)
	if err != nil {
		return nil, err
	}

	// Make the resolved skills natively discoverable by the CLI under
	// <workspace>/.claude/skills so `claude` registers and can trigger them.
	if !req.NoSkills {
		if err := materializeSkills(workspaceDir, req); err != nil {
			return nil, fmt.Errorf("failed to materialize skills: %w", err)
		}
	}

	workDir, err := ResolveWorkDir(workspaceDir, req.WorkDir)
	if err != nil {
		return nil, err
	}

	modelID := req.ModelID
	if modelID == "" {
		modelID = e.defaultModelID
	}
	modelID = normalizeClaudeModel(modelID)

	// Materialize any MCP servers configured on the request (plus the grade-tool
	// bridge, when present) into a temp config file the CLI loads via
	// --mcp-config. Kept outside the workspace so it is not captured into the
	// graded workspace files, and removed after the run.
	bridgeURL := ""
	if gradeBridge != nil {
		bridgeURL = gradeBridge.url
	}
	mcpConfigPath, err := writeMCPConfig(req.MCPServers, bridgeURL)
	if err != nil {
		return nil, err
	}
	if mcpConfigPath != "" {
		defer func() { _ = os.Remove(mcpConfigPath) }()
	}

	// Render the eval's instruction files (and, when grade tools are bridged, the
	// judge guidance that tells the model to call them) to a temp file passed via
	// --append-system-prompt-file. Kept outside the workspace so it is not
	// captured into the graded files, and removed after the run.
	sysPromptPath, err := writeSystemPromptFile(req.Instructions, req.Tools)
	if err != nil {
		return nil, err
	}
	if sysPromptPath != "" {
		defer func() { _ = os.Remove(sysPromptPath) }()
	}

	args := buildClaudeArgs(req, modelID, workspaceDir, mcpConfigPath, sysPromptPath)

	// Derive a cancelable run context so the first-event watchdog and the
	// skill-invocation early-cancel can kill the subprocess independently of the
	// caller's ctx. Killing the process closes stdout, so parseClaudeStream ends.
	runCtx, cancelRun := context.WithCancelCause(ctx)
	defer cancelRun(nil)

	cmd := exec.CommandContext(runCtx, e.binPath, args...)
	cmd.Dir = workDir
	cmd.Env = filteredEnv(os.Environ())

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to create stdout pipe: %w", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start claude CLI: %w", err)
	}

	// FirstEventTimeout watchdog: if the CLI emits no stream event within the
	// budget, treat it as a session-start hang and kill it with a distinct cause.
	// It is disarmed the moment the first event lands — the overall ctx deadline
	// governs the rest of the (legitimately long) turn.
	firstEvent := make(chan struct{})
	var firstEventOnce sync.Once
	signalFirstEvent := func() { firstEventOnce.Do(func() { close(firstEvent) }) }
	if req.FirstEventTimeout > 0 {
		timer := time.AfterFunc(req.FirstEventTimeout, func() { cancelRun(errClaudeFirstEventTimeout) })
		go func() {
			select {
			case <-firstEvent:
			case <-runCtx.Done():
			}
			timer.Stop()
		}()
	}

	// CancelOnSkillInvocation: end the turn as soon as the awaited skill fires,
	// rather than waiting for the agent to finish. The kill is expected, not a
	// failure — reconciled below via the run-context cause.
	skillInvoked := make(chan struct{})
	var skillOnce sync.Once
	signalSkill := func() { skillOnce.Do(func() { close(skillInvoked) }) }
	if req.CancelOnSkillInvocation {
		go func() {
			select {
			case <-skillInvoked:
				cancelRun(errClaudeCanceledForSkill)
			case <-runCtx.Done():
			}
		}()
	}

	parsed, parseErr := parseClaudeStream(stdout, streamHooks{
		onFirstEvent:   signalFirstEvent,
		onSkillInvoked: signalSkill,
	})
	waitErr := cmd.Wait()

	// Reconcile why the run ended. Our own aborts surface as runCtx causes; a
	// caller cancellation surfaces on ctx itself.
	cause := context.Cause(runCtx)
	canceledForSkill := errors.Is(cause, errClaudeCanceledForSkill)

	// Honor a genuine caller cancellation (not our internal skill early-cancel,
	// which leaves the parent ctx untouched).
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	if errors.Is(cause, errClaudeFirstEventTimeout) {
		return nil, fmt.Errorf("session start timeout: no first event within %s (claude launched but produced no stream output): %w", req.FirstEventTimeout, errClaudeFirstEventTimeout)
	}
	if parseErr != nil {
		return nil, fmt.Errorf("failed to parse claude stream output: %w", parseErr)
	}

	resp := &ExecutionResponse{
		FinalOutput:      parsed.FinalOutput,
		ModelID:          modelID,
		SkillInvocations: parsed.SkillInvocations,
		ToolCalls:        parsed.ToolCalls,
		Events:           buildClaudeTranscript(parsed, req.Message),
		ErrorMsg:         parsed.ErrorMsg,
		Success:          parsed.Success,
		WorkspaceDir:     workspaceDir,
		SessionID:        parsed.SessionID,
		Usage:            parsed.Usage,
		DurationMs:       time.Since(start).Milliseconds(),
	}

	switch {
	case canceledForSkill:
		// Expected early termination: the awaited skill fired and was recorded,
		// so the killed-process error is not a failure. Report success.
		resp.Success = true
		resp.ErrorMsg = ""
	case waitErr != nil && resp.ErrorMsg == "" && !resp.Success:
		// A non-zero exit with no parsed result line means the CLI failed before
		// emitting a usable turn; surface stderr so the failure is diagnosable.
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = waitErr.Error()
		}
		resp.ErrorMsg = msg
	}

	if !req.SkipWorkspaceCapture {
		resp.WorkspaceFiles = captureWorkspaceFilesExcludingClaude(workspaceDir)
	}

	if resp.SessionID == "" {
		// Synthetic key for usage bookkeeping only (no persisted session file);
		// the CLI's real session/project dirs are purged by workspace in Shutdown.
		resp.SessionID = fmt.Sprintf("claude-session-%d", start.UnixNano())
	}
	if resp.Usage != nil {
		e.mu.Lock()
		e.usage[resp.SessionID] = resp.Usage
		e.mu.Unlock()
	}

	return resp, nil
}

// resolveWorkspace reuses an existing workspace (for follow-up prompts) or
// creates a fresh temp workspace and materializes request resources and git
// resources into it, mirroring MockEngine/CopilotEngine.
func (e *ClaudeCodeEngine) resolveWorkspace(ctx context.Context, req *ExecutionRequest) (string, error) {
	if req.WorkspaceDir != "" {
		return req.WorkspaceDir, nil
	}

	workspaceDir, err := os.MkdirTemp("", "waza-claude-*")
	if err != nil {
		return "", fmt.Errorf("failed to create claude workspace: %w", err)
	}

	e.mu.Lock()
	e.workspaces = append(e.workspaces, workspaceDir)
	e.mu.Unlock()

	if err := setupWorkspaceResources(workspaceDir, req.Resources); err != nil {
		return "", fmt.Errorf("failed to setup workspace resources: %w", err)
	}

	gitRes, err := CloneGitResources(ctx, req.GitResources, workspaceDir)
	if err != nil {
		return "", fmt.Errorf("failed to materialize git resources: %w", err)
	}
	if len(gitRes) > 0 {
		e.mu.Lock()
		e.gitResources = append(e.gitResources, gitRes...)
		e.mu.Unlock()
	}

	return workspaceDir, nil
}

// Shutdown cleans up git resources and removes workspaces. It is idempotent and
// safe to call multiple times.
func (e *ClaudeCodeEngine) Shutdown(ctx context.Context) error {
	e.mu.Lock()
	gitResources := e.gitResources
	workspaces := e.workspaces
	e.gitResources = nil
	e.workspaces = nil
	e.mu.Unlock()

	for _, gr := range gitResources {
		if err := gr.Cleanup(ctx); err != nil {
			slog.Warn("failed to cleanup claude git resource", "error", err)
		}
	}

	// Purge the CLI's per-project dirs for our workspaces unless the caller asked
	// to keep workspaces (in which case the sessions are useful for debugging
	// alongside them).
	if !e.keepWorkspace {
		purgeWorkspaceProjects(workspaces)
	}

	for _, ws := range workspaces {
		if ws == "" {
			continue
		}
		if e.keepWorkspace {
			fmt.Fprintf(os.Stderr, "Workspace preserved: %s\n", ws)
			continue
		}
		if err := os.RemoveAll(ws); err != nil {
			return fmt.Errorf("failed to remove claude workspace %s: %w", ws, err)
		}
	}
	return nil
}

// SessionUsage returns the recorded usage stats for sessionID, or nil.
func (e *ClaudeCodeEngine) SessionUsage(sessionID string) *models.UsageStats {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.usage[sessionID]
}

// copilotModelAlias matches Anthropic model names written in the Copilot SDK's
// dotted form, e.g. "claude-haiku-4.5" / "claude-opus-4.6" / "claude-sonnet-4.6".
// The Claude CLI rejects that form with a 404 — it expects the hyphenated ID
// ("claude-haiku-4-5") or a bare alias ("haiku").
var copilotModelAlias = regexp.MustCompile(`^(claude-(?:opus|sonnet|haiku)-\d+)\.(\d+)$`)

// normalizeClaudeModel rewrites a Copilot-style dotted model name into the
// hyphenated form the Claude CLI accepts, so eval specs authored for the
// copilot-sdk executor (which use names like "claude-haiku-4.5") run unchanged
// under claude-code. Aliases ("haiku"), already-hyphenated IDs, dated snapshot
// IDs, and non-Anthropic names are passed through untouched.
func normalizeClaudeModel(model string) string {
	return copilotModelAlias.ReplaceAllString(model, "$1-$2")
}

// buildClaudeArgs assembles the CLI arguments for a headless eval run. It is a
// standalone function so the exact flag set can be unit-tested.
func buildClaudeArgs(req *ExecutionRequest, modelID, workspaceDir, mcpConfigPath, sysPromptPath string) []string {
	args := []string{
		"-p", req.Message,
		"--output-format", "stream-json",
		"--verbose",
		"--permission-mode", "bypassPermissions",
		"--add-dir", workspaceDir,
	}
	// Load only the eval's MCP servers (‑‑strict keeps the host's global servers
	// out for reproducibility).
	if mcpConfigPath != "" {
		args = append(args, "--mcp-config", mcpConfigPath, "--strict-mcp-config")
	}
	// Multi-turn evals (static follow-ups and the responder loop) carry the prior
	// turn's SessionID; resume that conversation so the agent keeps its context.
	if req.SessionID != "" {
		args = append(args, "--resume", req.SessionID)
	}
	// A fresh ephemeral session (e.g. the judge run) leaves no persisted session
	// file. A normal turn is persisted so a later turn can --resume it.
	if req.EphemeralSession && req.SessionID == "" {
		args = append(args, "--no-session-persistence")
	}
	if modelID != "" {
		args = append(args, "--model", modelID)
	}
	// Instruction files go via --append-system-prompt-file, not the inline
	// --append-system-prompt, so a large set cannot exceed the single-argument
	// limit (MAX_ARG_STRLEN, ~128 KiB on Linux) and fail exec with E2BIG. The
	// caller writes and removes that file.
	if sysPromptPath != "" {
		args = append(args, "--append-system-prompt-file", sysPromptPath)
	}
	return args
}

// buildMCPConfig renders the request's MCP servers into the JSON document the
// Claude CLI loads via --mcp-config: {"mcpServers": {name: {...}}}. It returns
// nil when there are no servers and no grade-tool bridge. The copilot SDK's
// concrete config types are translated to the CLI's stdio/http shapes
// (command/args/env/cwd or url/headers). Unsupported types are skipped with a
// warning. When bridgeURL is non-empty, the in-process grade-tool bridge is added
// as an HTTP MCP server under gradeBridgeServerName so the judge can call the
// prompt grader's set_waza_grade_* tools.
func buildMCPConfig(servers map[string]copilot.MCPServerConfig, bridgeURL string) ([]byte, error) {
	if len(servers) == 0 && bridgeURL == "" {
		return nil, nil
	}
	out := make(map[string]any, len(servers)+1)
	if bridgeURL != "" {
		out[gradeBridgeServerName] = map[string]any{"type": "http", "url": bridgeURL}
	}
	for name, cfg := range servers {
		switch c := cfg.(type) {
		case copilot.MCPStdioServerConfig:
			m := map[string]any{"type": "stdio", "command": c.Command}
			if len(c.Args) > 0 {
				m["args"] = c.Args
			}
			if len(c.Env) > 0 {
				m["env"] = c.Env
			}
			if c.WorkingDirectory != "" {
				m["cwd"] = c.WorkingDirectory
			}
			out[name] = m
		case copilot.MCPHTTPServerConfig:
			m := map[string]any{"type": "http", "url": c.URL}
			if len(c.Headers) > 0 {
				m["headers"] = c.Headers
			}
			out[name] = m
		default:
			slog.Warn("skipping MCP server of unsupported type for claude-code", "server", name)
		}
	}
	if len(out) == 0 {
		return nil, nil
	}
	return json.Marshal(map[string]any{"mcpServers": out})
}

// writeMCPConfig writes the request's MCP servers (plus the grade-tool bridge,
// when bridgeURL is non-empty) to a temporary --mcp-config file and returns its
// path (empty when there is nothing to configure). The caller is responsible for
// removing the file after the run.
func writeMCPConfig(servers map[string]copilot.MCPServerConfig, bridgeURL string) (string, error) {
	data, err := buildMCPConfig(servers, bridgeURL)
	if err != nil {
		return "", fmt.Errorf("failed to build MCP config: %w", err)
	}
	if data == nil {
		return "", nil
	}
	f, err := os.CreateTemp("", "waza-mcp-*.json")
	if err != nil {
		return "", fmt.Errorf("failed to create MCP config file: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return "", fmt.Errorf("failed to write MCP config: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// writeSystemPromptFile renders the eval's instruction files — and, when grade
// tools are being bridged, the judge guidance that tells the model to call them
// — into a temporary file passed to the CLI via --append-system-prompt-file,
// returning its path (empty when there is nothing to write). A file is used
// rather than an inline --append-system-prompt value because a large instruction
// set would exceed the OS single-argument limit (MAX_ARG_STRLEN, ~128 KiB on
// Linux) and fail the exec with E2BIG. The caller is responsible for removing the
// file after the run.
func writeSystemPromptFile(instructions []InstructionFile, tools []copilot.Tool) (string, error) {
	sys := buildInstructionSystemMessage(instructions)
	if guidance := buildGradeToolGuidance(tools); guidance != "" {
		if sys != "" {
			sys += "\n\n"
		}
		sys += guidance
	}
	if sys == "" {
		return "", nil
	}
	f, err := os.CreateTemp("", "waza-claude-sys-*.txt")
	if err != nil {
		return "", fmt.Errorf("failed to create system prompt file: %w", err)
	}
	if _, err := f.WriteString(sys); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return "", fmt.Errorf("failed to write system prompt file: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// filteredEnv returns env with ANTHROPIC_API_KEY removed so the CLI uses the
// subscription OAuth token rather than metered API billing. It is a standalone
// function so the invariant is unit-testable.
func filteredEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if strings.HasPrefix(kv, apiKeyEnv+"=") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// materializeSkills makes the request's resolved skill directories discoverable
// by the Claude CLI by linking each skill into <workspace>/.claude/skills/<name>.
// The CLI populates its skills registry from this location; references/ and
// scripts/ resolve from the linked skill's real directory. Symlinks are used so
// large skill trees aren't copied; copy is used as a fallback when symlinking
// fails (e.g. cross-device).
//
// When skill_directories is not configured in the eval spec (req.SkillPaths is
// empty), materializeSkills falls back to walking up from req.SourceDir looking
// for a directory that contains req.SkillName/SKILL.md. This allows evals that
// omit skill_directories to still work when the skill lives in a sibling
// directory of the eval tree (e.g. .claude/skills/<name>/ alongside
// .claude/skills/aidd-eval/).
func materializeSkills(workspaceDir string, req *ExecutionRequest) error {
	skillsRoot := filepath.Join(workspaceDir, ".claude", "skills")

	linked := false
	for _, dir := range claudeSkillDirs(workspaceDir, req) {
		for _, sd := range discoverSkillDefs(dir) {
			ok, err := linkSkillInto(skillsRoot, sd)
			if err != nil {
				return err
			}
			linked = linked || ok
		}
	}

	// Fallback: when skill_directories is absent (req.SkillPaths empty) and we
	// know the target skill name, walk up from req.SourceDir until we find a
	// directory that contains <skillName>/SKILL.md, then materialize from there.
	// This covers layouts like .claude/skills/<name>/ where the skill sits
	// alongside the eval directory rather than inside a skills_directories path.
	if !linked && req.SkillName != "" && req.SourceDir != "" {
		if parentDir := findSkillDirByName(req.SkillName, req.SourceDir); parentDir != "" {
			slog.Debug("materializing skills via walk-up fallback", "parent_dir", parentDir, "skill", req.SkillName)
			for _, sd := range discoverSkillDefs(parentDir) {
				ok, err := linkSkillInto(skillsRoot, sd)
				if err != nil {
					return err
				}
				linked = linked || ok
			}
		}
	}

	if linked {
		slog.Debug("materialized skills for claude CLI", "skills_root", skillsRoot)
	}
	return nil
}

// linkSkillInto materializes one skill definition under skillsRoot as
// skillsRoot/<name> via a symlink (falling back to a recursive copy when
// symlinking is unavailable, e.g. cross-device). It reports whether a new
// link/copy was created (false when the name is unsafe or already present).
//
// sd.Name originates from untrusted SKILL.md frontmatter, so it is validated to
// be a single, safe path segment: a hostile name such as "../../evil" is
// rejected rather than allowed to escape skillsRoot when joined into dest.
func linkSkillInto(skillsRoot string, sd skillDefinition) (bool, error) {
	if sd.Name == "" || sd.Name == "." || sd.Name == ".." || sd.Name != filepath.Base(sd.Name) {
		slog.Warn("skipping skill with unsafe name", "name", sd.Name, "dir", sd.Dir)
		return false, nil
	}
	if err := os.MkdirAll(skillsRoot, 0o755); err != nil {
		return false, err
	}
	dest := filepath.Join(skillsRoot, sd.Name)
	if _, err := os.Lstat(dest); err == nil {
		return false, nil // already linked (e.g. duplicate across dirs)
	}
	src, err := filepath.Abs(sd.Dir)
	if err != nil {
		src = sd.Dir
	}
	if err := os.Symlink(src, dest); err != nil {
		if copyErr := copyDir(src, dest); copyErr != nil {
			return false, fmt.Errorf("linking skill %q: symlink failed (%v) and copy failed: %w", sd.Name, err, copyErr)
		}
	}
	return true, nil
}

// findSkillDirByName walks up from baseDir looking for the first ancestor
// directory that contains a subdirectory named skillName with a SKILL.md file
// inside. It returns that ancestor directory so discoverSkillDefs can scan it
// for all sibling skills. Returns "" if none is found within maxWalkDepth steps.
func findSkillDirByName(skillName, baseDir string) string {
	const maxWalkDepth = 8
	current := filepath.Clean(baseDir)
	for i := 0; i < maxWalkDepth; i++ {
		candidate := filepath.Join(current, skillName, "SKILL.md")
		if _, err := os.Stat(candidate); err == nil {
			return current
		}
		parent := filepath.Dir(current)
		if parent == current {
			break // reached filesystem root
		}
		current = parent
	}
	return ""
}

// claudeSkillDirs returns the ordered, de-duplicated set of directories to scan
// for skills: the base directory followed by the request's explicit SkillPaths.
// It mirrors the copilot engine's getSkillDirs but is kept local so the Claude
// engine has no compile dependency on copilot.go internals (which upstream may
// refactor freely).
func claudeSkillDirs(base string, req *ExecutionRequest) []string {
	dirs := []string{base}
	seen := map[string]bool{base: true}
	for _, path := range req.SkillPaths {
		if seen[path] {
			continue
		}
		seen[path] = true
		dirs = append(dirs, path)
	}
	return dirs
}

// discoverSkillDefs returns the skill definitions found at dir: either a direct
// SKILL.md in dir, or one level of subdirectories each containing a SKILL.md.
// This mirrors buildSkillSystemMessage's discovery (copilot.go).
func discoverSkillDefs(dir string) []skillDefinition {
	if sd := loadSkillDefinition(dir); sd != nil {
		return []skillDefinition{*sd}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var defs []skillDefinition
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasPrefix(name, ".") || name == "node_modules" || name == "vendor" {
			continue
		}
		if sd := loadSkillDefinition(filepath.Join(dir, name)); sd != nil {
			defs = append(defs, *sd)
		}
	}
	return defs
}

// copyDir recursively copies src to dst. Used as a fallback when symlinking a
// skill directory fails.
func copyDir(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
}

// captureWorkspaceFilesExcludingClaude is captureWorkspaceFiles but skips the
// injected .claude/ directory so linked skill trees don't pollute the captured
// workspace files that graders inspect.
func captureWorkspaceFilesExcludingClaude(dir string) map[string][]byte {
	all := captureWorkspaceFiles(dir)
	if all == nil {
		return nil
	}
	for k := range all {
		if k == ".claude" || strings.HasPrefix(k, ".claude/") {
			delete(all, k)
		}
	}
	return all
}
