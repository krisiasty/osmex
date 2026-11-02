package osclient

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// ClientConfig configures a Client.
type ClientConfig struct {
	Auth           *Auth
	HTTP           *http.Client
	Region         string
	Interface      string
	UserAgent      string
	MaxConcurrent  int
	RequestTimeout time.Duration
	Sleep          func(ctx context.Context, d time.Duration) error // nil → timer honouring ctx
	AllowInsecure  bool                                             // allow plain-http service endpoints
}

// Client issues authenticated requests to OpenStack services. A semaphore shared by all its
// services bounds the number of concurrent requests.
type Client struct {
	cfg ClientConfig
	sem chan struct{}
}

// ServiceSpec identifies a service in the catalog and how to call it.
type ServiceSpec struct {
	Types        []string // catalog types, preferred first
	Suffix       string   // version path appended to the catalog URL, e.g. "/v2.0"
	Microversion string   // full OpenStack-API-Version header value, e.g. "compute 2.96"
}

// ServiceClient issues requests to one service.
type ServiceClient struct {
	c    *Client
	spec ServiceSpec
}

const (
	maxRetries    = 3
	maxBodyLength = 64 << 20
)

// NewClient returns a Client for cfg.
func NewClient(cfg ClientConfig) *Client {
	if cfg.Sleep == nil {
		cfg.Sleep = sleep
	}
	cfg.HTTP = withRedirectPolicy(cfg.HTTP)
	return &Client{cfg: cfg, sem: make(chan struct{}, max(cfg.MaxConcurrent, 1))}
}

// Service returns a client for the service described by spec.
func (c *Client) Service(spec ServiceSpec) *ServiceClient { return &ServiceClient{c: c, spec: spec} }

// Token returns the token requests are made with, authenticating if needed. A failure is reported to the context's
// recorder, like a failed request.
func (s *ServiceClient) Token(ctx context.Context) (*Token, error) {
	tok, err := s.c.cfg.Auth.Token(ctx)
	if err != nil {
		recordError(ctx, err)
	}
	return tok, err
}

// Get requests path (relative to the service's versioned endpoint) and decodes the JSON response
// into out, unless out is nil. It re-authenticates once on HTTP 401 and retries 429 and 503
// responses, honouring Retry-After. Every failed attempt is reported to the context's recorder.
func (s *ServiceClient) Get(ctx context.Context, path string, query url.Values, out any) error {
	reauthenticated := false
	for retries := 0; ; {
		tok, err := s.c.cfg.Auth.Token(ctx)
		if err != nil {
			recordError(ctx, err)
			return err
		}
		base, err := tok.Catalog.EndpointURL(s.spec.Types, s.c.cfg.Region, s.c.cfg.Interface)
		if err != nil {
			recordError(ctx, err)
			return err
		}
		u := JoinURL(base, s.spec.Suffix) + path
		// Never send a token unencrypted unless explicitly allowed.
		if err := requireHTTPS(u, s.c.cfg.AllowInsecure); err != nil {
			recordError(ctx, err)
			return err
		}
		if len(query) > 0 {
			u += "?" + query.Encode()
		}
		status, header, body, err := s.c.do(ctx, u, tok.ID, s.spec.Microversion)
		if err != nil {
			recordError(ctx, err)
			return err
		}
		if status/100 == 2 {
			if out == nil {
				return nil
			}
			if err := json.Unmarshal(body, out); err != nil {
				err = &DecodeError{Err: err}
				recordError(ctx, err)
				return err
			}
			return nil
		}
		herr := &HTTPError{StatusCode: status, Method: http.MethodGet, Path: path, Body: string(body)}
		recordError(ctx, herr)
		switch {
		case status == http.StatusUnauthorized && !reauthenticated:
			s.c.cfg.Auth.Invalidate(tok.ID)
			reauthenticated = true
			continue
		case (status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable) && retries < maxRetries:
			wait := retryAfter(header.Get("Retry-After"), retries, time.Now())
			if deadline, ok := ctx.Deadline(); ok && time.Now().Add(wait).After(deadline) {
				return herr
			}
			if err := s.c.cfg.Sleep(ctx, wait); err != nil {
				return err
			}
			retries++
			continue
		}
		return herr
	}
}

func (c *Client) do(ctx context.Context, u, token, microversion string) (int, http.Header, []byte, error) {
	select {
	case c.sem <- struct{}{}:
	case <-ctx.Done():
		return 0, nil, nil, ctx.Err()
	}
	defer func() { <-c.sem }()

	if c.cfg.RequestTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.cfg.RequestTimeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, nil, nil, err
	}
	req.Header.Set("X-Auth-Token", token)
	req.Header.Set("Accept", "application/json")
	if c.cfg.UserAgent != "" {
		req.Header.Set("User-Agent", c.cfg.UserAgent)
	}
	if microversion != "" {
		req.Header.Set("OpenStack-API-Version", microversion)
	}
	resp, err := c.cfg.HTTP.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	// The body is read in full below; an error closing it carries no information.
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyLength))
	if err != nil {
		return 0, nil, nil, err
	}
	return resp.StatusCode, resp.Header, body, nil
}

func retryAfter(header string, retries int, now time.Time) time.Duration {
	if s, err := strconv.Atoi(header); err == nil && s >= 0 {
		return time.Duration(s) * time.Second
	}
	if t, err := http.ParseTime(header); err == nil {
		return max(t.Sub(now), 0)
	}
	return time.Second << retries
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
