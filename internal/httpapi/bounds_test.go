package httpapi_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// filler is an endless run of one byte, so a test can offer a body of any
// size without allocating it.
type filler byte

func (f filler) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(f)
	}
	return len(p), nil
}

// countingReader records how much of a body the server actually pulled.
type countingReader struct {
	r    io.Reader
	read int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.read += int64(n)
	return n, err
}

// oversizedJSON is a well-formed {"<field>":"aaaa…"} of roughly size bytes.
func oversizedJSON(field string, size int64) *countingReader {
	return &countingReader{r: io.MultiReader(
		strings.NewReader(`{"`+field+`":"`),
		io.LimitReader(filler('a'), size),
		strings.NewReader(`"}`),
	)}
}

const mib = 1 << 20

// Every request body is capped once, in Routes. The grant exchange takes no
// credential, so before the cap anyone could make burrow buffer a body of any
// size; it read all of it and then answered 401 as if the grant were merely
// wrong. The requests go straight at the handler so the test can count what
// the server read.
func TestRequestBodyIsCapped(t *testing.T) {
	e := newTestEnv(t)
	handler := e.srv.Config.Handler

	const sent = 32 * mib
	// The cap is 10 MiB; a decoder may overshoot by a buffer, never by much.
	const mostRead = 11 * mib

	tooLarge := func(t *testing.T, rec *httptest.ResponseRecorder) {
		t.Helper()
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("an over-limit body must 413, got %d: %s", rec.Code, rec.Body)
		}
		var payload any
		if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
			t.Fatalf("the 413 must carry the JSON error shape: %v: %s", err, rec.Body)
		}
		if code := nonEmptyString(t, mapOf(t, payload), "error"); code != "request_too_large" {
			t.Fatalf("error code must be request_too_large, got %q", code)
		}
	}

	t.Run("unauthenticated exchange, length undeclared", func(t *testing.T) {
		body := oversizedJSON("grant", sent)
		req := httptest.NewRequest(http.MethodPost, "/api/den/v1/auth/desktop-handoff/exchange", body)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		tooLarge(t, rec)
		if body.read > mostRead {
			t.Fatalf("burrow read %d bytes of a %d-byte body; the cap must stop it near 10 MiB", body.read, int64(sent))
		}
	})

	t.Run("unauthenticated exchange, length declared", func(t *testing.T) {
		body := oversizedJSON("grant", sent)
		req := httptest.NewRequest(http.MethodPost, "/api/den/v1/auth/desktop-handoff/exchange", body)
		req.ContentLength = sent + int64(len(`{"grant":""}`))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		tooLarge(t, rec)
		if body.read != 0 {
			t.Fatalf("a declared over-limit length must be refused unread, burrow read %d bytes", body.read)
		}
	})

	// The mint ignores a body it cannot decode, so an over-limit one must not
	// fall through to a minted grant.
	t.Run("handoff mint", func(t *testing.T) {
		body := oversizedJSON("desktopScheme", sent)
		req := httptest.NewRequest(http.MethodPost, "/api/den/v1/auth/desktop-handoff", body)
		req.Header.Set("Authorization", "Bearer "+e.token)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		tooLarge(t, rec)
		if body.read > mostRead {
			t.Fatalf("burrow read %d bytes of a %d-byte body; the cap must stop it near 10 MiB", body.read, int64(sent))
		}
	})

	// The tighter caps stay: the MCP endpoint still stops at its own 4 MiB,
	// well inside the 10 MiB every route gets.
	t.Run("the MCP endpoint keeps its tighter cap", func(t *testing.T) {
		var minted any
		e.doJSON(t, http.MethodPost, "/api/den/v1/mcp/token", map[string]any{}, &minted, true)
		token := nonEmptyString(t, mapOf(t, minted), "token")

		body := oversizedJSON("method", 8*mib)
		req := httptest.NewRequest(http.MethodPost, "/mcp/agent", body)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("an MCP body over 4 MiB must 400, got %d: %s", rec.Code, rec.Body)
		}
		if body.read > 5*mib {
			t.Fatalf("the MCP endpoint read %d bytes; its 4 MiB cap must still apply", body.read)
		}
	})

	// A body of ordinary size is untouched by the cap.
	t.Run("an ordinary body is served", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/den/v1/auth/desktop-handoff/exchange",
			strings.NewReader(`{"grant":"not-a-real-grant"}`))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("an unknown grant must still 401, got %d: %s", rec.Code, rec.Body)
		}
	})
}

// With BURROW_BASE_URL unset the origin is derived from the request, and the
// forwarded scheme used to be taken verbatim: X-Forwarded-Proto: javascript
// came back as "javascript://<host>/api/den" in denApiUrl, and the same string
// feeds the MCP resource, the deep link's denBaseUrl, and whether the console
// cookie is Secure. Only http and https are schemes burrow can be served on;
// anything else falls back to the connection's own.
func TestOriginAcceptsOnlyHTTPSchemes(t *testing.T) {
	e := newTestEnv(t)
	handler := e.srv.Config.Handler

	cases := []struct {
		name      string
		forwarded string
		want      string
	}{
		{"no header", "", "http://burrow.example/api/den"},
		{"https from the proxy", "https", "https://burrow.example/api/den"},
		{"first hop of a list", "https, http", "https://burrow.example/api/den"},
		{"scheme case", "HTTPS", "https://burrow.example/api/den"},
		{"explicit http", "http", "http://burrow.example/api/den"},
		{"javascript", "javascript", "http://burrow.example/api/den"},
		{"file", "file", "http://burrow.example/api/den"},
		{"a scheme with a payload", "javascript:alert(1)//", "http://burrow.example/api/den"},
		{"a bad first hop is not skipped for a later one", "javascript, https", "http://burrow.example/api/den"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/runtime-config", nil)
			req.Host = "burrow.example"
			if tc.forwarded != "" {
				req.Header.Set("X-Forwarded-Proto", tc.forwarded)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d: %s", rec.Code, rec.Body)
			}
			var payload any
			if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
				t.Fatal(err)
			}
			if got := nonEmptyString(t, mapOf(t, payload), "denApiUrl"); got != tc.want {
				t.Fatalf("X-Forwarded-Proto %q: denApiUrl want %q, got %q", tc.forwarded, tc.want, got)
			}
		})
	}
}
