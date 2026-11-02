package osclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"testing"
	"time"
)

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func TestErrorCode(t *testing.T) {
	_ = json.Unmarshal([]byte("<html>"), &struct{}{})
	jerr := json.Unmarshal([]byte("<html>"), &struct{}{})
	if _, ok := errors.AsType[*json.SyntaxError](jerr); !ok {
		t.Fatal("setup: expected json.SyntaxError")
	}
	cases := []struct {
		err  error
		want string
	}{
		{&HTTPError{StatusCode: 403}, "403"},
		{fmt.Errorf("x: %w", &HTTPError{StatusCode: 503}), "503"},
		{fmt.Errorf("x: %w", ErrNoEndpoint), "catalog"},
		{fmt.Errorf("x: %w", ErrAmbiguousEndpoint), "catalog"},
		{context.Canceled, "other"},
		{&authError{err: errors.New("bad")}, "auth"},
		{context.DeadlineExceeded, "timeout"},
		{&url.Error{Op: "Get", URL: "u", Err: timeoutErr{}}, "timeout"},
		{&url.Error{Op: "Get", URL: "u", Err: &net.OpError{Op: "dial", Err: errors.New("refused")}}, "network"},
		{&DecodeError{Err: jerr}, "decode"},
		{errors.New("other"), "other"},
	}
	for _, c := range cases {
		if got := ErrorCode(c.err); got != c.want {
			t.Errorf("ErrorCode(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}

func TestHTTPErrorMessageTruncatesBody(t *testing.T) {
	body := make([]byte, 1000)
	for i := range body {
		body[i] = 'a'
	}
	e := &HTTPError{StatusCode: 500, Body: string(body)}
	if len(e.Error()) > 260 {
		t.Errorf("message too long: %d", len(e.Error()))
	}
}

type recorder struct{ codes []string }

func (r *recorder) RecordError(code string) { r.codes = append(r.codes, code) }

func TestRecorderIgnoresCancelled(t *testing.T) {
	r := &recorder{}
	ctx, cancel := context.WithCancel(WithRecorder(t.Context(), r))
	cancel() // shutdown: the request failed because the exporter is stopping, not because of the API
	RecordErrorCode(ctx, "other")
	recordError(ctx, context.Canceled)
	expired, cancelExpired := context.WithTimeout(WithRecorder(t.Context(), r), -time.Second)
	defer cancelExpired()
	recordError(expired, context.DeadlineExceeded) // the poll deadline is a real failure
	if fmt.Sprint(r.codes) != "[timeout]" {
		t.Errorf("codes = %v", r.codes)
	}
}

func TestRecorder(t *testing.T) {
	RecordErrorCode(t.Context(), "500") // no recorder: no panic
	r := &recorder{}
	ctx := WithRecorder(t.Context(), r)
	RecordErrorCode(ctx, "500")
	recordError(ctx, &HTTPError{StatusCode: 404})
	if fmt.Sprint(r.codes) != "[500 404]" {
		t.Errorf("codes = %v", r.codes)
	}
}

func TestHTTPErrorMessage(t *testing.T) {
	for _, tt := range []struct {
		name string
		err  HTTPError
		want string
	}{
		{"barbican", HTTPError{StatusCode: 403, Method: "GET", Path: "/secrets",
			Body: `{"code": 403, "title": "Forbidden", "description": "Secret(s) retrieval attempt not allowed - please review your user/project privileges"}`},
			"GET /secrets: HTTP 403 Forbidden: Secret(s) retrieval attempt not allowed - please review your user/project privileges"},
		{"nova", HTTPError{StatusCode: 403, Method: "GET", Path: "/servers/detail",
			Body: `{"forbidden": {"code": 403, "message": "Policy doesn't allow os_compute_api:servers:detail to be performed."}}`},
			"GET /servers/detail: HTTP 403 Forbidden: Policy doesn't allow os_compute_api:servers:detail to be performed."},
		{"neutron", HTTPError{StatusCode: 404, Method: "GET", Path: "/ports",
			Body: `{"NeutronError": {"type": "HTTPNotFound", "message": "The resource could not be found.", "detail": ""}}`},
			"GET /ports: HTTP 404 Not Found: The resource could not be found."},
		{"octavia", HTTPError{StatusCode: 403, Method: "GET", Path: "/loadbalancers",
			Body: `{"faultcode": "Client", "faultstring": "Policy does not allow this request to be performed.", "debuginfo": null}`},
			"GET /loadbalancers: HTTP 403 Forbidden: Policy does not allow this request to be performed."},
		{"keystone", HTTPError{StatusCode: 401, Method: "POST", Path: "/v3/auth/tokens",
			Body: `{"error": {"code": 401, "title": "Unauthorized", "message": "The request you have made requires authentication."}}`},
			"POST /v3/auth/tokens: HTTP 401 Unauthorized: The request you have made requires authentication."},
		{"html from a proxy", HTTPError{StatusCode: 503, Method: "GET", Path: "/x",
			Body: "<html><body><h1>503 Service Unavailable</h1>\nNo server is available.</body></html>"},
			"GET /x: HTTP 503 Service Unavailable"},
		{"plain text", HTTPError{StatusCode: 502, Method: "GET", Path: "/x", Body: "upstream connect error\nreset reason: overflow"},
			"GET /x: HTTP 502 Bad Gateway: upstream connect error"},
		{"multi-line message", HTTPError{StatusCode: 400, Method: "GET", Path: "/x", Body: `{"badRequest": {"message": "Invalid  input\n for field"}}`},
			"GET /x: HTTP 400 Bad Request: Invalid input for field"},
		{"json without message", HTTPError{StatusCode: 500, Method: "GET", Path: "/x", Body: `{"code": 500}`},
			"GET /x: HTTP 500 Internal Server Error"},
		{"empty body", HTTPError{StatusCode: 500}, "HTTP 500 Internal Server Error"},
		{"unknown status", HTTPError{StatusCode: 599}, "HTTP 599"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.err.Error(); got != tt.want {
				t.Errorf("Error() =\n%q\nwant\n%q", got, tt.want)
			}
		})
	}
}
