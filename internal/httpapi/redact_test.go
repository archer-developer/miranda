package httpapi

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda-llm/llmtest"
)

// fakeRedactor stands in for internal/redact — the engine is tested there.
type fakeRedactor struct{}

func (fakeRedactor) Redact(s string) string {
	return strings.ReplaceAll(s, "665533", "******")
}

// TestServer_HandleInput_RedactsLoggedBody guards the earliest sink for user
// text in the whole process. handleInput logs the raw body before auth and
// before parsing, and s.logger reaches logs/miranda.log, stdout — hence the
// systemd journal, which Miranda's own rotation settings do not govern — and
// the web UI's app_log tab. Masking the stores alone would have left a pin
// code sitting in the journal indefinitely.
func TestServer_HandleInput_RedactsLoggedBody(t *testing.T) {
	provider := llmtest.New("local", llmtest.Response{Text: "записала"})
	o, _, _ := newTestOrchestrator(t, provider)

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))
	server := NewServer(o, o.Hub(), "", nil, logger, nil, nil)
	server.SetRedactor(fakeRedactor{})

	ts := httptest.NewServer(server)
	defer ts.Close()

	raw := []byte(`{"source":"ha_assist","user_id":"alex","text":"пин-код от телефона Ани 665533"}`)
	resp, err := http.Post(ts.URL+"/api/v1/input", "application/json", bytes.NewReader(raw))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	logged := logBuf.String()
	require.Contains(t, logged, "received input request")
	require.NotContains(t, logged, "665533", "the pin must not reach the app log")
	// The body's shape is what makes this line useful for diagnosing a
	// misconfigured client, and masking values leaves that intact.
	require.Contains(t, logged, `\"source\":\"ha_assist\"`)
}

// TestServer_HandleInput_RedactsLoggedBodyEvenWhenUnauthorized — the log line
// happens before the auth check on purpose, so the masking must too.
func TestServer_HandleInput_RedactsLoggedBodyEvenWhenUnauthorized(t *testing.T) {
	provider := llmtest.New("local")
	o, _, _ := newTestOrchestrator(t, provider)

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))
	server := NewServer(o, o.Hub(), "secret-token", nil, logger, nil, nil)
	server.SetRedactor(fakeRedactor{})

	ts := httptest.NewServer(server)
	defer ts.Close()

	raw := []byte(`{"source":"ha_assist","text":"пин-код 665533"}`)
	resp, err := http.Post(ts.URL+"/api/v1/input", "application/json", bytes.NewReader(raw))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	logged := logBuf.String()
	require.Contains(t, logged, "received input request")
	require.NotContains(t, logged, "665533")
}

// TestServer_WithoutRedactorLogsVerbatim — redaction stays optional, and the
// existing diagnostic behavior is unchanged when it is off.
func TestServer_WithoutRedactorLogsVerbatim(t *testing.T) {
	provider := llmtest.New("local", llmtest.Response{Text: "ок"})
	o, _, _ := newTestOrchestrator(t, provider)

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))
	server := NewServer(o, o.Hub(), "", nil, logger, nil, nil)

	ts := httptest.NewServer(server)
	defer ts.Close()

	raw := []byte(`{"source":"ha_assist","user_id":"alex","text":"пин-код 665533"}`)
	resp, err := http.Post(ts.URL+"/api/v1/input", "application/json", bytes.NewReader(raw))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Contains(t, logBuf.String(), "665533")
}
