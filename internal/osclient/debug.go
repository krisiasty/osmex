package osclient

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// loggingTransport logs every HTTP exchange at debug level: one line when the request is sent and one when its
// response body is closed, or when it fails. Headers and bodies are never logged, so tokens and the credential secret
// stay out of the log, and URLs are redacted (safeURL).
type loggingTransport struct {
	base          http.RoundTripper
	logger        *slog.Logger
	nextRequestID atomic.Uint64
}

// NewLoggingTransport returns base wrapped so that every exchange is logged to logger at debug level, with the name of
// the poller that made it. Nothing is logged while logger has debug disabled.
func NewLoggingTransport(base http.RoundTripper, logger *slog.Logger) http.RoundTripper {
	return &loggingTransport{base: base, logger: logger}
}

func (t *loggingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	if !t.logger.Enabled(ctx, slog.LevelDebug) {
		return t.base.RoundTrip(req)
	}
	attrs := []any{"request_id", t.nextRequestID.Add(1)}
	// The recorder is the poller whose poll made the request; requests outside a poll, such as token refreshes
	// started by Auth.Run, have none.
	if p, ok := ctx.Value(recorderKey{}).(interface{ Name() string }); ok {
		attrs = append(attrs, "poller", p.Name())
	}
	attrs = append(attrs, "method", req.Method, "url", safeURL(req.URL))
	t.logger.DebugContext(ctx, "http request", append(attrs, "content_length", req.ContentLength)...)

	started := time.Now()
	resp, err := t.base.RoundTrip(req)
	timeToHeaders := time.Since(started)
	if err != nil {
		t.logger.DebugContext(ctx, "http response", append(attrs, "duration", timeToHeaders, "error", err)...)
		return nil, err
	}
	attrs = append(attrs, "status", resp.Status, "status_code", resp.StatusCode, "content_length", resp.ContentLength,
		"content_type", resp.Header.Get("Content-Type"))
	if loc := resp.Header.Get("Location"); loc != "" {
		attrs = append(attrs, "location", safeLocation(loc))
	}
	resp.Body = &loggingBody{ReadCloser: resp.Body, logger: t.logger, ctx: ctx, attrs: attrs, started: started, timeToHeaders: timeToHeaders}
	return resp, nil
}

// loggingBody logs the response when the body is closed, with how much of it was read and the first read error.
type loggingBody struct {
	io.ReadCloser
	logger        *slog.Logger
	ctx           context.Context
	attrs         []any
	started       time.Time
	timeToHeaders time.Duration
	bytesRead     atomic.Int64
	complete      atomic.Bool
	readErr       atomic.Pointer[error] // a body cut short fails on Read, not on Close
	once          sync.Once
}

func (b *loggingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.bytesRead.Add(int64(n))
	switch {
	case err == io.EOF:
		b.complete.Store(true)
	case err != nil:
		b.readErr.CompareAndSwap(nil, &err)
	}
	return n, err
}

func (b *loggingBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(func() {
		attrs := append(b.attrs, "response_bytes", b.bytesRead.Load(), "time_to_headers", b.timeToHeaders,
			"duration", time.Since(b.started), "body_complete", b.complete.Load())
		if readErr := b.readErr.Load(); readErr != nil {
			attrs = append(attrs, "read_error", *readErr)
		}
		if err != nil {
			attrs = append(attrs, "close_error", err)
		}
		b.logger.DebugContext(b.ctx, "http response", attrs...)
	})
	return err
}

func safeLocation(location string) string {
	u, err := url.Parse(location)
	if err != nil {
		return "<invalid>"
	}
	return safeURL(u)
}

// safeURL returns u without user information or fragment, and with the values of query parameters whose names suggest
// a secret replaced by REDACTED. A fragment never reaches the server, but a redirect can carry one (#access_token=…).
func safeURL(u *url.URL) string {
	redacted := *u
	redacted.User = nil
	redacted.Fragment, redacted.RawFragment = "", ""
	query := redacted.Query()
	for key := range query {
		lower := strings.ToLower(key)
		for _, s := range []string{"token", "signature", "secret", "password", "credential"} {
			if strings.Contains(lower, s) || lower == "sig" || lower == "jwt" {
				query.Set(key, "REDACTED")
				break
			}
		}
	}
	redacted.RawQuery = query.Encode()
	return redacted.String()
}
