package osclient_test

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/krisiasty/osmex/internal/osclient"
	"github.com/krisiasty/osmex/internal/ostest"
)

// syncBuffer is a bytes.Buffer safe for the concurrent writes of the HTTP client and the test.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func debugLogger(level slog.Level) (*slog.Logger, *syncBuffer) {
	var buf syncBuffer
	return slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: level})), &buf
}

type namedRecorder struct{ name string }

func (namedRecorder) RecordError(string) {}
func (r namedRecorder) Name() string    { return r.name }

func get(t *testing.T, client *http.Client, req *http.Request) {
	t.Helper()
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}

func TestLoggingTransportExchange(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"servers": []}`))
	}))
	t.Cleanup(srv.Close)
	logger, buf := debugLogger(slog.LevelDebug)
	client := &http.Client{Transport: osclient.NewLoggingTransport(http.DefaultTransport, logger)}

	ctx := osclient.WithRecorder(t.Context(), namedRecorder{"nova-servers"})
	u := strings.Replace(srv.URL, "http://", "http://user:pass@", 1) + "/servers?marker=m1&token=abc&X-Signature=s&limit=10"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Auth-Token", "secret-token-value")
	get(t, client, req)

	logs := buf.String()
	for _, want := range []string{
		`msg="http request" request_id=1 poller=nova-servers method=GET`,
		`msg="http response" request_id=1 poller=nova-servers method=GET`,
		"marker=m1", "token=REDACTED", "X-Signature=REDACTED", "limit=10",
		"status_code=200", "response_bytes=15", "content_type=application/json", "body_complete=true", "time_to_headers=", "duration=",
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("missing %q in:\n%s", want, logs)
		}
	}
	if strings.Contains(logs, " host=") {
		t.Errorf("host logged apart from the URL:\n%s", logs)
	}
	for _, leak := range []string{"user:pass", "pass@", "abc", "secret-token-value", "=s&", "=s "} {
		if strings.Contains(logs, leak) {
			t.Errorf("log contains %q:\n%s", leak, logs)
		}
	}
}

func TestLoggingTransportError(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	u := srv.URL + "/v3/auth/tokens"
	srv.Close()
	logger, buf := debugLogger(slog.LevelDebug)
	client := &http.Client{Transport: osclient.NewLoggingTransport(http.DefaultTransport, logger)}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, u, strings.NewReader(`{"secret": "x"}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Do(req); err == nil {
		t.Fatal("request to a closed server succeeded")
	}
	logs := buf.String()
	if !strings.Contains(logs, `msg="http request" request_id=1 method=POST`) || !strings.Contains(logs, "content_length=15") ||
		!strings.Contains(logs, `msg="http response" request_id=1 method=POST`) || !strings.Contains(logs, "error=") {
		t.Errorf("failed exchange not logged:\n%s", logs)
	}
	if strings.Contains(logs, `"secret"`) {
		t.Errorf("request body logged:\n%s", logs)
	}
}

func TestLoggingTransportRedirect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/old" {
			http.Redirect(w, r, "/new?sig=abc", http.StatusFound)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(srv.Close)
	logger, buf := debugLogger(slog.LevelDebug)
	client := &http.Client{Transport: osclient.NewLoggingTransport(http.DefaultTransport, logger)}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/old", nil)
	if err != nil {
		t.Fatal(err)
	}
	get(t, client, req)
	logs := buf.String()
	if strings.Count(logs, `msg="http response"`) != 2 || !strings.Contains(logs, "status_code=302") ||
		!strings.Contains(logs, `location="/new?sig=REDACTED"`) || strings.Contains(logs, "abc") {
		t.Errorf("redirect not logged as two exchanges with a redacted location:\n%s", logs)
	}
}

func TestLoggingTransportSilentWithoutDebug(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	t.Cleanup(srv.Close)
	logger, buf := debugLogger(slog.LevelInfo)
	client := &http.Client{Transport: osclient.NewLoggingTransport(http.DefaultTransport, logger)}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	get(t, client, req)
	if buf.String() != "" {
		t.Errorf("logged below debug level:\n%s", buf)
	}
}

func TestLoggingTransportKeepsCredentialsOut(t *testing.T) {
	// A full authentication and API call through the client: neither the credential secret nor a token may be logged.
	cloud := ostest.New(t, "compute")
	cloud.Handle("GET /compute/v2.1/servers/detail", func(w http.ResponseWriter, r *http.Request) {
		ostest.WriteJSON(w, 200, map[string]any{"servers": []any{}})
	})
	logger, buf := debugLogger(slog.LevelDebug)
	httpClient := cloud.Server.Client()
	httpClient.Transport = osclient.NewLoggingTransport(httpClient.Transport, logger)
	auth := osclient.NewAuth(osclient.AuthConfig{URL: cloud.AuthURL(), CredentialID: "cred-id", CredentialSecret: "cred-secret", HTTP: httpClient})
	client := osclient.NewClient(osclient.ClientConfig{Auth: auth, HTTP: httpClient, Region: "RegionOne", Interface: "public", MaxConcurrent: 1})
	if err := client.Service(osclient.ServiceSpec{Types: []string{"compute"}}).Get(t.Context(), "/servers/detail", nil, nil); err != nil {
		t.Fatal(err)
	}
	logs := buf.String()
	if strings.Count(logs, `msg="http response"`) != 2 || !strings.Contains(logs, "/identity/v3/auth/tokens") {
		t.Errorf("want the token request and the API call logged:\n%s", logs)
	}
	for _, leak := range []string{"cred-secret", "tok-"} {
		if strings.Contains(logs, leak) {
			t.Errorf("log contains %q:\n%s", leak, logs)
		}
	}
}

func TestLoggingTransportDropsFragments(t *testing.T) {
	// A fragment never reaches the server, but a redirect's Location can carry one, and the client keeps it in the next
	// request's URL.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/old" {
			w.Header().Set("Location", "/new#access_token=leaked")
			w.WriteHeader(http.StatusFound)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(srv.Close)
	logger, buf := debugLogger(slog.LevelDebug)
	client := &http.Client{Transport: osclient.NewLoggingTransport(http.DefaultTransport, logger)}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/old", nil)
	if err != nil {
		t.Fatal(err)
	}
	get(t, client, req)
	logs := buf.String()
	if strings.Count(logs, `msg="http request"`) != 2 || strings.Contains(logs, "leaked") || strings.Contains(logs, "#") {
		t.Errorf("fragment logged:\n%s", logs)
	}
}

func TestLoggingTransportReadError(t *testing.T) {
	// A body cut short fails on Read, not on Close; the exchange log must say so.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		_, _ = w.Write([]byte("only ten.."))
	}))
	t.Cleanup(srv.Close)
	logger, buf := debugLogger(slog.LevelDebug)
	client := &http.Client{Transport: osclient.NewLoggingTransport(http.DefaultTransport, logger)}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(resp.Body); err == nil {
		t.Fatal("truncated body read without error")
	}
	_ = resp.Body.Close()
	logs := buf.String()
	if !strings.Contains(logs, "response_bytes=10") || !strings.Contains(logs, "body_complete=false") ||
		!strings.Contains(logs, `read_error="unexpected EOF"`) {
		t.Errorf("read error not logged:\n%s", logs)
	}
}
