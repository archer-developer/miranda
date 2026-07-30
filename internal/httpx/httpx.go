// Package httpx holds the small piece of HTTP-client plumbing shared by
// Miranda's own minimal API clients (internal/telegram, internal/tavily):
// marshal a JSON body, POST it, and read back a size-capped response body.
// Each client still speaks its own response envelope on top of this
// (Telegram's {ok,result}, Tavily's {detail}) — that part isn't shared,
// since the two don't actually agree on a shape — only the transport
// mechanics (and the size cap, which neither client enforced on its own
// before this existed) are common enough to factor out.
package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// DefaultMaxResponseBytes bounds how much of a response body PostJSON reads
// into memory when the caller doesn't need a different limit — generous
// enough for any real API response, small enough that a misbehaving
// upstream (or an intermediate proxy/WAF returning a large HTML error page
// instead of the expected JSON) can't force unbounded memory use.
const DefaultMaxResponseBytes = 2 << 20 // 2 MiB

// PostJSON marshals payload, POSTs it to url with headers applied on top of
// "Content-Type: application/json", and returns the response's status code
// and body — read through an io.LimitReader capped at maxBytes (use
// DefaultMaxResponseBytes if the caller has no reason to pick a different
// limit). Callers are responsible for interpreting status/body themselves;
// this only owns the transport round trip, not any particular response
// envelope.
func PostJSON(ctx context.Context, client *http.Client, url string, headers map[string]string, payload any, maxBytes int64) (status int, body []byte, err error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return 0, nil, fmt.Errorf("httpx: marshal request body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(encoded))
	if err != nil {
		return 0, nil, fmt.Errorf("httpx: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("httpx: do request: %w", err)
	}
	defer resp.Body.Close()

	body, err = io.ReadAll(io.LimitReader(resp.Body, maxBytes))
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("httpx: read response body: %w", err)
	}
	return resp.StatusCode, body, nil
}
