package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/microsoft/waza/internal/config"
	"github.com/microsoft/waza/internal/execution"
	"github.com/microsoft/waza/internal/models"
	"github.com/microsoft/waza/internal/orchestration"
	"github.com/stretchr/testify/require"
)

const runSignalHelperEnv = "WAZA_RUN_SIGNAL_HELPER"
const runSignalSpecEnv = "WAZA_RUN_SIGNAL_SPEC"

type blockingBenchmarkRunner struct{}

func (r *blockingBenchmarkRunner) OnProgress(orchestration.ProgressListener) {}

func (r *blockingBenchmarkRunner) RunBenchmark(ctx context.Context) (*models.EvaluationOutcome, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestRunCommand_CancelsOnSIGTERM(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SIGTERM is not supported on Windows")
	}
	resetRunGlobals()

	dir := t.TempDir()
	taskDir := filepath.Join(dir, "tasks")
	require.NoError(t, os.MkdirAll(taskDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(taskDir, "task.yaml"), []byte(`
id: signal-task
name: Signal Task
inputs:
  prompt: "test"
`), 0o644))

	specPath := filepath.Join(dir, "eval.yaml")
	require.NoError(t, os.WriteFile(specPath, []byte(`
name: signal-cancel-test
skill: test-skill
version: "1.0"
config:
  trials_per_task: 1
  timeout_seconds: 30
  executor: mock
  model: test-model
tasks:
  - "tasks/*.yaml"
`), 0o644))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestRunCommand_SignalCancelsBenchmarkHelperProcess")
	cmd.Env = append(os.Environ(),
		runSignalHelperEnv+"=1",
		runSignalSpecEnv+"="+specPath,
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	require.NoError(t, cmd.Start())
	time.Sleep(200 * time.Millisecond)
	require.NoError(t, cmd.Process.Signal(syscall.SIGTERM))
	require.NoError(t, cmd.Wait(), "run should exit cleanly after SIGTERM; stdout=%s stderr=%s", stdout.String(), stderr.String())
}

func TestRunCommand_SignalCancelsBenchmarkHelperProcess(t *testing.T) {
	if os.Getenv(runSignalHelperEnv) != "1" {
		return
	}

	resetRunGlobals()
	newBenchmarkRunner = func(cfg *config.EvalConfig, engine execution.AgentEngine, opts ...orchestration.RunnerOption) benchmarkRunner {
		return &blockingBenchmarkRunner{}
	}

	specPath := os.Getenv(runSignalSpecEnv)
	if specPath == "" {
		fmt.Fprintln(os.Stderr, "missing signal test spec path")
		os.Exit(2)
	}

	cmd := newRunCommand()
	cmd.SetArgs([]string{specPath})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)

	err := cmd.Execute()
	if err == nil {
		fmt.Fprintln(os.Stderr, "expected interrupt to cancel the run")
		os.Exit(3)
	}
	if !strings.Contains(err.Error(), "context canceled") {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(4)
	}
	os.Exit(0)
}
