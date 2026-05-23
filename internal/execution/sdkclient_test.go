package execution

import (
	"context"
	"testing"

	copilot "github.com/github/copilot-sdk/go"
)

// stubClient is a no-op CopilotClient used only to verify SharedClient
// returns the same instance and Shutdown is idempotent.
type stubClient struct{ stops int }

func (s *stubClient) CreateSession(context.Context, *copilot.SessionConfig) (CopilotSession, error) {
	return nil, nil
}
func (s *stubClient) GetAuthStatus(context.Context) (*copilot.GetAuthStatusResponse, error) {
	return nil, nil
}
func (s *stubClient) Start(context.Context) error { return nil }
func (s *stubClient) Stop() error                 { s.stops++; return nil }
func (s *stubClient) ResumeSessionWithOptions(context.Context, string, *copilot.ResumeSessionConfig) (CopilotSession, error) {
	return nil, nil
}
func (s *stubClient) DeleteSession(context.Context, string) error { return nil }
func (s *stubClient) ListModels(context.Context) ([]copilot.ModelInfo, error) {
	return nil, nil
}

func TestSharedClient_ReturnsSameInstance(t *testing.T) {
	resetSharedClientForTest()
	t.Cleanup(resetSharedClientForTest)

	stub := &stubClient{}
	sharedConstruct = func(*copilot.ClientOptions) CopilotClient { return stub }
	t.Cleanup(func() { sharedConstruct = newCopilotClient })

	a := SharedClient(SharedClientOptions{})
	b := SharedClient(SharedClientOptions{LogLevel: "debug"}) // ignored after first call
	if a != b {
		t.Fatalf("expected SharedClient to return the same instance")
	}
	if a == nil {
		t.Fatalf("SharedClient returned nil")
	}
}

func TestShutdownSharedClient_Idempotent(t *testing.T) {
	resetSharedClientForTest()
	t.Cleanup(resetSharedClientForTest)

	stub := &stubClient{}
	sharedConstruct = func(*copilot.ClientOptions) CopilotClient { return stub }
	t.Cleanup(func() { sharedConstruct = newCopilotClient })

	_ = SharedClient(SharedClientOptions{})
	if err := ShutdownSharedClient(context.Background()); err != nil {
		t.Fatalf("first shutdown: %v", err)
	}
	if err := ShutdownSharedClient(context.Background()); err != nil {
		t.Fatalf("second shutdown should be no-op: %v", err)
	}
	if stub.stops != 1 {
		t.Fatalf("expected client Stop to be called once, got %d", stub.stops)
	}
}

func TestShutdownSharedClient_NoClientNeverConstructed(t *testing.T) {
	resetSharedClientForTest()
	t.Cleanup(resetSharedClientForTest)

	if err := ShutdownSharedClient(context.Background()); err != nil {
		t.Fatalf("expected no error when client never built, got %v", err)
	}
}

func TestShutdownSharedClient_BeforeConstructDoesNotPoisonShutdown(t *testing.T) {
	resetSharedClientForTest()
	t.Cleanup(resetSharedClientForTest)

	stub := &stubClient{}
	sharedConstruct = func(*copilot.ClientOptions) CopilotClient { return stub }
	t.Cleanup(func() { sharedConstruct = newCopilotClient })

	if err := ShutdownSharedClient(context.Background()); err != nil {
		t.Fatalf("shutdown before construction: %v", err)
	}
	_ = SharedClient(SharedClientOptions{})
	if err := ShutdownSharedClient(context.Background()); err != nil {
		t.Fatalf("shutdown after construction: %v", err)
	}
	if stub.stops != 1 {
		t.Fatalf("expected client Stop to be called once, got %d", stub.stops)
	}
}
