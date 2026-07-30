package httpx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPostJSON_SendsHeadersAndBody(t *testing.T) {
	var gotAuth, gotContentType string
	var gotBody string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotContentType = r.Header.Get("Content-Type")
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		gotBody = string(buf)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	status, body, err := PostJSON(context.Background(), server.Client(), server.URL,
		map[string]string{"Authorization": "Bearer test-token"}, map[string]any{"foo": "bar"}, DefaultMaxResponseBytes)

	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, status)
	require.JSONEq(t, `{"ok":true}`, string(body))
	require.Equal(t, "Bearer test-token", gotAuth)
	require.Equal(t, "application/json", gotContentType)
	require.Contains(t, gotBody, `"foo":"bar"`)
}

func TestPostJSON_CapsResponseBodySize(t *testing.T) {
	huge := strings.Repeat("a", 1000)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(huge))
	}))
	defer server.Close()

	status, body, err := PostJSON(context.Background(), server.Client(), server.URL, nil, map[string]any{}, 100)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status)
	require.Len(t, body, 100)
}
