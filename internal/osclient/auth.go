package osclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Token is a Keystone token with its service catalog.
type Token struct {
	ID        string
	ExpiresAt time.Time
	Catalog   Catalog
	Project   Project
	UserID    string // the user the application credential belongs to
}

// Project is the project a token is scoped to; application credentials are bound to one.
type Project struct {
	ID         string
	Name       string
	DomainID   string
	DomainName string
}

// AuthConfig configures an Auth.
type AuthConfig struct {
	URL              string
	CredentialID     string
	CredentialSecret string
	HTTP             *http.Client
	UserAgent        string
	Timeout          time.Duration    // per authentication request; 0 → 30s
	AllowInsecure    bool             // allow a plain-http Keystone URL
	Now              func() time.Time // nil → time.Now
	Logger           *slog.Logger     // nil → slog.Default()
}

// Auth obtains and refreshes application-credential tokens. It is safe for concurrent use;
// concurrent callers share one in-flight authentication.
type Auth struct {
	cfg       AuthConfig
	tokensURL string

	mu       sync.Mutex
	tok      *Token
	inflight *flight

	ready     chan struct{}
	readyOnce sync.Once
}

const (
	refreshMargin      = 5 * time.Minute
	minValidity        = time.Minute
	defaultAuthTimeout = 30 * time.Second
	minRetryDelay      = 5 * time.Second
	maxRetryDelay      = 5 * time.Minute
	authBodyLimit      = 4 << 20
	errorBodyLimit     = 4 << 10
)

// NewAuth returns an Auth for cfg.
func NewAuth(cfg AuthConfig) *Auth {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultAuthTimeout
	}
	cfg.HTTP = withRedirectPolicy(cfg.HTTP)
	return &Auth{cfg: cfg, tokensURL: tokensURL(cfg.URL), ready: make(chan struct{})}
}

// Token returns a token valid for at least another minute, authenticating if needed.
func (a *Auth) Token(ctx context.Context) (*Token, error) { return a.get(ctx, false) }

// Invalidate drops the cached token if it still has the given id, e.g. after an HTTP 401.
func (a *Auth) Invalidate(tokenID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.tok != nil && a.tok.ID == tokenID {
		a.tok = nil
	}
}

// Up reports whether an unexpired token is held.
func (a *Auth) Up() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.tok != nil && a.cfg.Now().Before(a.tok.ExpiresAt)
}

// Project returns the project of the most recently obtained token, or the zero Project before the
// first successful authentication.
func (a *Auth) Project() Project {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.tok == nil {
		return Project{}
	}
	return a.tok.Project
}

// Catalog returns the service catalog of the most recently obtained token, even if it has
// expired, or nil before the first successful authentication.
func (a *Auth) Catalog() Catalog {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.tok == nil {
		return nil
	}
	return a.tok.Catalog
}

// Ready is closed after the first successful authentication.
func (a *Auth) Ready() <-chan struct{} { return a.ready }

// Run authenticates with exponential backoff until it succeeds, then refreshes the token about
// five minutes before it expires (at half its remaining lifetime for short-lived tokens).
// It returns when ctx is done.
func (a *Auth) Run(ctx context.Context) {
	backoff := minRetryDelay
	force := false
	for {
		var wait time.Duration
		tok, err := a.get(ctx, force)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			a.cfg.Logger.Warn("authentication failed", "err", err, "retry_in", backoff)
			wait = backoff
			backoff = min(backoff*2, maxRetryDelay)
		} else {
			backoff = minRetryDelay
			wait = max(refreshDelay(a.cfg.Now(), tok.ExpiresAt), time.Second)
			force = true
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

func refreshDelay(now, expires time.Time) time.Duration {
	remaining := expires.Sub(now)
	if remaining <= 0 {
		return 0
	}
	return max(remaining-refreshMargin, remaining/2)
}

func tokensURL(authURL string) string {
	u := strings.TrimRight(authURL, "/")
	u = strings.TrimSuffix(u, "/v3")
	return u + "/v3/auth/tokens"
}

// flight is one in-progress authentication shared by every caller that needs a token meanwhile.
type flight struct {
	done chan struct{}
	tok  *Token
	err  error
}

// get returns the cached token when it is valid for at least minValidity (unless force), and
// otherwise joins or starts an authentication. The authentication runs detached from the caller's
// cancellation and bounded by the configured timeout, so one caller giving up neither fails the
// other waiters nor leaves a hung request blocking later ones; each caller waits only as long as
// its own ctx allows.
func (a *Auth) get(ctx context.Context, force bool) (*Token, error) {
	a.mu.Lock()
	if !force && a.tok != nil && a.cfg.Now().Add(minValidity).Before(a.tok.ExpiresAt) {
		tok := a.tok
		a.mu.Unlock()
		return tok, nil
	}
	f := a.inflight
	if f == nil {
		f = &flight{done: make(chan struct{})}
		a.inflight = f
		go a.fly(context.WithoutCancel(ctx), f)
	}
	a.mu.Unlock()

	select {
	case <-f.done:
		return f.tok, f.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (a *Auth) fly(ctx context.Context, f *flight) {
	ctx, cancel := context.WithTimeout(ctx, a.cfg.Timeout)
	defer cancel()
	f.tok, f.err = a.authenticate(ctx)

	a.mu.Lock()
	a.inflight = nil
	if f.err == nil {
		a.tok = f.tok
	}
	a.mu.Unlock()
	if f.err == nil {
		a.readyOnce.Do(func() { close(a.ready) })
		a.cfg.Logger.Debug("authenticated", "expires_at", f.tok.ExpiresAt)
	}
	close(f.done)
}

func (a *Auth) authenticate(ctx context.Context) (*Token, error) {
	// Never send the credential secret unencrypted unless explicitly allowed.
	if err := requireHTTPS(a.tokensURL, a.cfg.AllowInsecure); err != nil {
		return nil, &authError{err: err}
	}
	var body bytes.Buffer
	req := map[string]any{"auth": map[string]any{"identity": map[string]any{
		"methods": []string{"application_credential"},
		"application_credential": map[string]string{
			"id":     a.cfg.CredentialID,
			"secret": a.cfg.CredentialSecret,
		},
	}}}
	if err := json.NewEncoder(&body).Encode(req); err != nil {
		return nil, &authError{err: err}
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.tokensURL, &body)
	if err != nil {
		return nil, &authError{err: err}
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Accept", "application/json")
	if a.cfg.UserAgent != "" {
		hreq.Header.Set("User-Agent", a.cfg.UserAgent)
	}
	resp, err := a.cfg.HTTP.Do(hreq)
	if err != nil {
		return nil, &authError{err: err}
	}
	// The body is read in full below; an error closing it carries no information.
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, errorBodyLimit))
		return nil, &authError{err: &HTTPError{StatusCode: resp.StatusCode, Method: http.MethodPost, Path: "/v3/auth/tokens", Body: string(b)}}
	}
	var out struct {
		Token struct {
			ExpiresAt string  `json:"expires_at"`
			Catalog   Catalog `json:"catalog"`
			Project   struct {
				ID     string `json:"id"`
				Name   string `json:"name"`
				Domain struct {
					ID   string `json:"id"`
					Name string `json:"name"`
				} `json:"domain"`
			} `json:"project"`
			User struct {
				ID string `json:"id"`
			} `json:"user"`
		} `json:"token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, authBodyLimit)).Decode(&out); err != nil {
		return nil, &authError{err: &DecodeError{Err: err}}
	}
	id := resp.Header.Get("X-Subject-Token")
	if id == "" {
		return nil, &authError{err: errors.New("response has no X-Subject-Token header")}
	}
	expires, err := ParseTime(out.Token.ExpiresAt)
	if err != nil || expires.IsZero() {
		return nil, &authError{err: errors.New("response has no valid expires_at")}
	}
	p := out.Token.Project
	return &Token{ID: id, ExpiresAt: expires, Catalog: out.Token.Catalog, Project: Project{
		ID: p.ID, Name: p.Name, DomainID: p.Domain.ID, DomainName: p.Domain.Name,
	}, UserID: out.Token.User.ID}, nil
}
