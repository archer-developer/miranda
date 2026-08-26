package agentloop

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	llm "github.com/archer-developer/miranda-llm"
	"github.com/archer-developer/miranda-llm/llmtest"
	"github.com/archer-developer/miranda-llm/llmtrace/anomaly"
)

// Fixture trace text mirroring a real gemini.Provider trace for a turn where
// the model called a tool that doesn't exist — the same shape
// analyze/describe.go's DescribeIncoming/ExtractToolCalls decode in
// production.
const (
	geminiQuestionRequest   = `{"contents":[{"role":"user","parts":[{"text":"turn on the kitchen light"}]}]}`
	geminiToolCallResponse  = `{"text":"","tool_calls":[{"Name":"no_such_tool","Arguments":"{}"}]}`
	geminiToolResultRequest = `{"contents":[` +
		`{"role":"user","parts":[{"text":"turn on the kitchen light"}]},` +
		`{"role":"model","parts":[{"functionCall":{"name":"no_such_tool","args":{}}}]},` +
		`{"role":"user","parts":[{"functionResponse":{"name":"no_such_tool","response":{"result":"error: mcp: no configured server matches tool \"no_such_tool\""}}}]}` +
		`]}`
	geminiFinalAnswerResponse = `{"text":"sorry, I couldn't do that"}`
)

func TestReportAnomalies_NilRecorderIsNoOp(t *testing.T) {
	var logBuf bytes.Buffer
	provider := llmtest.New("local", llmtest.Response{Text: "hi"})
	o, _, _ := newTestOrchestrator(t, provider)
	o.SetLogger(slog.New(slog.NewTextHandler(&logBuf, nil)))
	o.SetAnomalyConfig(AnomalyConfig{Dir: t.TempDir()})

	require.NotPanics(t, func() {
		o.reportAnomalies("conv-1", nil, anomaly.Outcome{})
	})
	require.Empty(t, logBuf.String())
}

func TestReportAnomalies_NoAnomaliesIsNoOp(t *testing.T) {
	var logBuf bytes.Buffer
	dir := filepath.Join(t.TempDir(), "anomalies")
	provider := llmtest.New("local", llmtest.Response{Text: "hi"})
	o, _, _ := newTestOrchestrator(t, provider)
	o.SetLogger(slog.New(slog.NewTextHandler(&logBuf, nil)))
	o.SetAnomalyConfig(AnomalyConfig{Dir: dir})

	recorder := anomaly.NewRecorder("conv-1")
	recorder.Trace(context.Background(), "gemini", geminiQuestionRequest, geminiFinalAnswerResponse, nil)

	o.reportAnomalies("conv-1", recorder, anomaly.Outcome{IterationCount: 1, MaxIterations: maxToolIterations})

	require.Empty(t, logBuf.String())
	_, err := os.Stat(dir)
	require.True(t, os.IsNotExist(err), "no anomalies dir should be created when nothing was found")
}

func TestReportAnomalies_WritesFileAndWarnsOnAnomaly(t *testing.T) {
	var logBuf bytes.Buffer
	dir := filepath.Join(t.TempDir(), "anomalies")
	provider := llmtest.New("local", llmtest.Response{Text: "hi"})
	o, _, _ := newTestOrchestrator(t, provider)
	o.SetLogger(slog.New(slog.NewTextHandler(&logBuf, nil)))
	o.SetAnomalyConfig(AnomalyConfig{Dir: dir})

	recorder := anomaly.NewRecorder("conv-1")
	recorder.Trace(context.Background(), "gemini", geminiQuestionRequest, geminiToolCallResponse, nil)
	recorder.Trace(context.Background(), "gemini", geminiToolResultRequest, geminiFinalAnswerResponse, nil)

	o.reportAnomalies("conv-1", recorder, anomaly.Outcome{IterationCount: 2, MaxIterations: maxToolIterations})

	require.Contains(t, logBuf.String(), "turn had anomalies")
	require.Contains(t, logBuf.String(), "unknown_tool")

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Contains(t, entries[0].Name(), "unknown_tool")

	content, err := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	require.NoError(t, err)
	require.Contains(t, string(content), "no_such_tool")
}

func TestReportAnomalies_FallsBackToTurnBlocksWhenLLMLogUnreadable(t *testing.T) {
	var logBuf bytes.Buffer
	dir := filepath.Join(t.TempDir(), "anomalies")
	provider := llmtest.New("local", llmtest.Response{Text: "hi"})
	o, _, _ := newTestOrchestrator(t, provider)
	o.SetLogger(slog.New(slog.NewTextHandler(&logBuf, nil)))
	o.SetAnomalyConfig(AnomalyConfig{Dir: dir, LLMLogPath: filepath.Join(t.TempDir(), "missing-llm.log")})

	recorder := anomaly.NewRecorder("conv-1")
	recorder.Trace(context.Background(), "gemini", geminiQuestionRequest, geminiToolCallResponse, nil)
	recorder.Trace(context.Background(), "gemini", geminiToolResultRequest, geminiFinalAnswerResponse, nil)

	o.reportAnomalies("conv-1", recorder, anomaly.Outcome{IterationCount: 2, MaxIterations: maxToolIterations})

	require.Contains(t, logBuf.String(), "re-reading llm.log for anomaly context failed")
	require.Contains(t, logBuf.String(), "turn had anomalies")

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1, "the anomaly file must still be written from the turn's own blocks")
}

// TestHandle_ExceedingMaxIterations_WritesAnomalyFileAndWarns exercises the
// full anomaly-detection wiring end to end through a real Handle() call — a
// Recorder attached via ctx (see orchestrator.go), Detect finding the
// iteration_cap anomaly from Outcome (filled in by runAgentLoop), and
// reportAnomalies (anomaly.go) writing a file and logging exactly one
// WARNING. Unlike TestReportAnomalies_* above (which drive reportAnomalies
// directly with hand-built Gemini-shaped blocks, since llmtest.FakeProvider
// traces requests via fmt.Sprintf("%+v", ...) rather than real provider
// JSON), this only asserts the iteration_cap/timeout kinds are reachable
// this way — those are the ones Detect derives from Outcome, not from
// parsing block content.
func TestHandle_ExceedingMaxIterations_WritesAnomalyFileAndWarns(t *testing.T) {
	script := make([]llmtest.Response, maxToolIterations)
	for i := range script {
		script[i] = llmtest.Response{ToolCall: &llm.ToolCall{ID: "call", Name: "remember_this", Arguments: `{"fact":"x"}`}}
	}
	provider := llmtest.New("local", script...)
	o, _, _ := newTestOrchestrator(t, provider)

	var logBuf bytes.Buffer
	dir := filepath.Join(t.TempDir(), "anomalies")
	o.SetLogger(slog.New(slog.NewTextHandler(&logBuf, nil)))
	o.SetAnomalyConfig(AnomalyConfig{Dir: dir})

	// Handle no longer surfaces this as an error — see orchestrator.go's
	// Handle: a failed agent loop (iteration cap, timeout, provider outage)
	// now falls back to an ordinary-looking reply instead of leaving the
	// caller (and the user) with nothing. Anomaly detection still fires
	// exactly the same either way, since reportAnomalies is deferred and
	// reads Outcome regardless of what Handle ultimately returns.
	resp, err := o.Handle(context.Background(), InputRequest{Source: "cli", UserID: "alex", Text: "keep going"})
	require.NoError(t, err)
	require.NotEmpty(t, resp.Reply)

	require.Contains(t, logBuf.String(), "turn had anomalies")
	require.Contains(t, logBuf.String(), "iteration_cap")

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Contains(t, entries[0].Name(), "iteration_cap")
}

func TestCleanOldAnomalies_DeletesOnlyFilesOlderThanMaxAge(t *testing.T) {
	dir := t.TempDir()
	provider := llmtest.New("local", llmtest.Response{Text: "hi"})
	o, _, _ := newTestOrchestrator(t, provider)
	o.SetAnomalyConfig(AnomalyConfig{Dir: dir, MaxAge: 24 * time.Hour})

	oldFile := filepath.Join(dir, "old_unknown_tool.log")
	freshFile := filepath.Join(dir, "fresh_unknown_tool.log")
	require.NoError(t, os.WriteFile(oldFile, []byte("old"), 0o644))
	require.NoError(t, os.WriteFile(freshFile, []byte("fresh"), 0o644))
	require.NoError(t, os.Chtimes(oldFile, time.Now().Add(-48*time.Hour), time.Now().Add(-48*time.Hour)))

	require.NoError(t, o.CleanOldAnomalies())

	_, err := os.Stat(oldFile)
	require.True(t, os.IsNotExist(err), "file older than MaxAge should be removed")
	_, err = os.Stat(freshFile)
	require.NoError(t, err, "file within MaxAge should be kept")
}

func TestCleanOldAnomalies_NoopWhenMaxAgeZeroOrDisabled(t *testing.T) {
	dir := t.TempDir()
	provider := llmtest.New("local", llmtest.Response{Text: "hi"})
	o, _, _ := newTestOrchestrator(t, provider)

	oldFile := filepath.Join(dir, "old_unknown_tool.log")
	require.NoError(t, os.WriteFile(oldFile, []byte("old"), 0o644))
	require.NoError(t, os.Chtimes(oldFile, time.Now().Add(-48*time.Hour), time.Now().Add(-48*time.Hour)))

	// MaxAge left at zero (anomaly detection disabled entirely, matching the
	// zero-value default from SetAnomalyConfig never being called).
	require.NoError(t, o.CleanOldAnomalies())
	_, err := os.Stat(oldFile)
	require.NoError(t, err, "cleanup must be a no-op when MaxAge is unset")

	// Dir configured but MaxAge still zero — same expectation.
	o.SetAnomalyConfig(AnomalyConfig{Dir: dir})
	require.NoError(t, o.CleanOldAnomalies())
	_, err = os.Stat(oldFile)
	require.NoError(t, err, "cleanup must be a no-op when MaxAge is unset even with Dir configured")
}

func TestCleanOldAnomalies_MissingDirIsNoOp(t *testing.T) {
	provider := llmtest.New("local", llmtest.Response{Text: "hi"})
	o, _, _ := newTestOrchestrator(t, provider)
	o.SetAnomalyConfig(AnomalyConfig{Dir: filepath.Join(t.TempDir(), "never-created"), MaxAge: 24 * time.Hour})

	require.NoError(t, o.CleanOldAnomalies())
}
