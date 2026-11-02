package osclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// HTTPError is a non-2xx response from an OpenStack API.
type HTTPError struct {
	StatusCode int
	Method     string // request method, e.g. GET
	Path       string // request path relative to the service endpoint, without query
	Body       string
}

// Error reads like "GET /secrets: HTTP 403 Forbidden: <message from the service>".
func (e *HTTPError) Error() string {
	var b strings.Builder
	if e.Path != "" {
		b.WriteString(e.Method + " " + e.Path + ": ")
	}
	b.WriteString("HTTP " + strconv.Itoa(e.StatusCode))
	if text := http.StatusText(e.StatusCode); text != "" {
		b.WriteString(" " + text)
	}
	if msg := errorMessage(e.Body); msg != "" {
		b.WriteString(": " + msg)
	}
	return b.String()
}

// messageKeys are the fields OpenStack services put an error's text in: Nova, Cinder, Neutron and
// Keystone use "message" (nested one level), Barbican "description", Octavia "faultstring".
var messageKeys = []string{"message", "description", "faultstring"}

// maxMessage bounds the service message in an error, in characters.
const maxMessage = 200

// errorMessage extracts the human-readable text from an error response body: the message field of a
// JSON body, or the first line of a plain-text one. HTML pages, usually from a proxy, yield nothing.
func errorMessage(body string) string {
	body = strings.TrimSpace(body)
	if body == "" || strings.HasPrefix(body, "<") {
		return ""
	}
	var doc map[string]any
	if json.Unmarshal([]byte(body), &doc) == nil {
		return tidy(findMessage(doc))
	}
	line, _, _ := strings.Cut(body, "\n")
	return tidy(line)
}

// findMessage looks for a message field at the top level, then one level down.
func findMessage(doc map[string]any) string {
	if msg := messageField(doc); msg != "" {
		return msg
	}
	for _, k := range slices.Sorted(maps.Keys(doc)) {
		if nested, ok := doc[k].(map[string]any); ok {
			if msg := messageField(nested); msg != "" {
				return msg
			}
		}
	}
	return ""
}

func messageField(m map[string]any) string {
	for _, k := range messageKeys {
		if s, ok := m[k].(string); ok && strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

// tidy collapses whitespace, including newlines, and bounds the length.
func tidy(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) > maxMessage {
		s = string([]rune(s)[:maxMessage]) + "…"
	}
	return s
}

// DecodeError wraps a failure to decode a response body.
type DecodeError struct{ Err error }

func (e *DecodeError) Error() string { return "decoding response: " + e.Err.Error() }
func (e *DecodeError) Unwrap() error { return e.Err }

// ErrNoEndpoint means the service catalog has no matching endpoint.
var ErrNoEndpoint = errors.New("service not in catalog")

// ErrAmbiguousEndpoint means a service has endpoints in several regions and no region was configured.
var ErrAmbiguousEndpoint = errors.New("service has endpoints in several regions")

// ErrInsecure means a request would have gone over plain HTTP without -insecure.
var ErrInsecure = errors.New("refusing unencrypted connection to OpenStack (use -insecure to allow it)")

// requireHTTPS returns ErrInsecure for a URL that is not https, unless allow is set. Only the scheme and host are
// named in the error, so credentials in the URL are never repeated.
func requireHTTPS(rawURL string, allow bool) error {
	if allow {
		return nil
	}
	u, err := url.Parse(rawURL)
	if err != nil || !strings.EqualFold(u.Scheme, "https") {
		host := "(unparsable URL)"
		if err == nil {
			host = u.Scheme + "://" + u.Host
		}
		return fmt.Errorf("%w: %s", ErrInsecure, host)
	}
	return nil
}

type authError struct{ err error }

func (e *authError) Error() string { return "authentication failed: " + e.err.Error() }
func (e *authError) Unwrap() error { return e.err }

// BaselineErrorCodes are the api_errors_total codes exported at 0 before they first occur, so that increase() counts
// the first one: every code ErrorCode returns other than an HTTP status, and the HTTP statuses APIs fail with most.
var BaselineErrorCodes = []string{
	"auth", "catalog", "insecure", "redirect", "timeout", "network", "decode", "other",
	"401", "403", "404", "429", "500", "502", "503", "504",
}

// ErrorCode classifies err for the api_errors_total code label.
func ErrorCode(err error) string {
	var (
		httpErr   *HTTPError
		authErr   *authError
		decodeErr *DecodeError
		netErr    net.Error
	)
	switch {
	case errors.Is(err, ErrInsecure):
		return "insecure"
	case errors.Is(err, ErrRedirect):
		return "redirect"
	case errors.As(err, &authErr):
		return "auth"
	case errors.As(err, &httpErr):
		return strconv.Itoa(httpErr.StatusCode)
	case errors.Is(err, ErrNoEndpoint), errors.Is(err, ErrAmbiguousEndpoint):
		return "catalog"
	case errors.As(err, &decodeErr):
		return "decode"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.As(err, &netErr):
		if netErr.Timeout() {
			return "timeout"
		}
		return "network"
	}
	return "other"
}

// ErrorRecorder receives the code of every failed API request.
type ErrorRecorder interface {
	RecordError(code string)
}

type recorderKey struct{}

// WithRecorder returns a context whose failed API requests are reported to r.
func WithRecorder(ctx context.Context, r ErrorRecorder) context.Context {
	return context.WithValue(ctx, recorderKey{}, r)
}

// RecordErrorCode reports code to the context's recorder, if any. Nothing is reported once ctx is cancelled: only
// shutdown cancels it, and a request it interrupts says nothing about the API. An expired deadline is still reported.
func RecordErrorCode(ctx context.Context, code string) {
	if errors.Is(ctx.Err(), context.Canceled) {
		return
	}
	if r, ok := ctx.Value(recorderKey{}).(ErrorRecorder); ok {
		r.RecordError(code)
	}
}

func recordError(ctx context.Context, err error) {
	RecordErrorCode(ctx, ErrorCode(err))
}
