// Package ostest provides a fake OpenStack cloud (Keystone plus service endpoints) for tests.
package ostest

import (
	"bytes"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/krisiasty/osmex/internal/osclient"
)

var update = flag.Bool("update", false, "rewrite golden files")

// Cloud is a fake OpenStack cloud. Service requests must carry a token it issued.
type Cloud struct {
	Server    *httptest.Server
	TokenTTL  time.Duration // default 1h
	AuthCalls atomic.Int32
	FailAuth  atomic.Bool
	FailAfter atomic.Int32 // when > 0, authentications after this many fail

	mux       *http.ServeMux
	types     []string
	tokens    atomic.Int32
	endpoints map[string]string // catalog URL overrides by service type
}

// New starts a fake cloud served over HTTPS whose catalog lists serviceTypes. Its clients trust the server's
// certificate; CACertFile writes it for code that loads OS_CACERT.
func New(t *testing.T, serviceTypes ...string) *Cloud {
	t.Helper()
	return newCloud(t, httptest.NewTLSServer, serviceTypes)
}

// NewInsecure starts the same fake cloud over plain HTTP, for testing -insecure.
func NewInsecure(t *testing.T, serviceTypes ...string) *Cloud {
	t.Helper()
	return newCloud(t, httptest.NewServer, serviceTypes)
}

func newCloud(t *testing.T, start func(http.Handler) *httptest.Server, serviceTypes []string) *Cloud {
	c := &Cloud{mux: http.NewServeMux(), types: serviceTypes, endpoints: map[string]string{}}
	c.mux.HandleFunc("POST /identity/v3/auth/tokens", c.handleAuth)
	c.Server = start(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/identity/") && !strings.HasPrefix(r.Header.Get("X-Auth-Token"), "tok-") {
			http.Error(w, "missing token", http.StatusUnauthorized)
			return
		}
		c.mux.ServeHTTP(w, r)
	}))
	t.Cleanup(c.Server.Close)
	return c
}

// SetEndpoint overrides the catalog URL of a service type.
func (c *Cloud) SetEndpoint(serviceType, url string) { c.endpoints[serviceType] = url }

// CACertFile writes the server's certificate as a PEM file for OS_CACERT and returns its path.
func (c *Cloud) CACertFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ca.pem")
	data := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Server.Certificate().Raw})
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// Endpoint returns the catalog URL of a service type.
func (c *Cloud) Endpoint(serviceType string) string {
	if url, ok := c.endpoints[serviceType]; ok {
		return url
	}
	suffix := ""
	switch serviceType {
	case "compute":
		suffix = "/v2.1"
	case "volumev3", "block-storage":
		suffix = "/v3/proj"
	}
	return c.Server.URL + "/" + serviceType + suffix
}

// Handle registers h for pattern, e.g. "GET /compute/v2.1/servers/detail".
func (c *Cloud) Handle(pattern string, h http.HandlerFunc) { c.mux.HandleFunc(pattern, h) }

// AuthURL returns the Keystone URL to configure.
func (c *Cloud) AuthURL() string { return c.Server.URL + "/identity" }

// Auth returns an authenticator for the cloud.
func (c *Cloud) Auth(t *testing.T) *osclient.Auth {
	return osclient.NewAuth(osclient.AuthConfig{
		URL:              c.AuthURL(),
		CredentialID:     "cred-id",
		CredentialSecret: "cred-secret",
		HTTP:             c.Server.Client(),
		UserAgent:        "osmex-test",
	})
}

func (c *Cloud) handleAuth(w http.ResponseWriter, r *http.Request) {
	c.AuthCalls.Add(1)
	var req struct {
		Auth struct {
			Identity struct {
				Methods []string `json:"methods"`
				Cred    struct {
					ID     string `json:"id"`
					Secret string `json:"secret"`
				} `json:"application_credential"`
			} `json:"identity"`
		} `json:"auth"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	id := req.Auth.Identity
	if c.FailAuth.Load() || (c.FailAfter.Load() > 0 && c.AuthCalls.Load() > c.FailAfter.Load()) || len(id.Methods) != 1 || id.Methods[0] != "application_credential" ||
		id.Cred.ID != "cred-id" || id.Cred.Secret != "cred-secret" {
		WriteJSON(w, http.StatusUnauthorized, map[string]any{"error": map[string]any{"code": 401}})
		return
	}
	ttl := c.TokenTTL
	if ttl == 0 {
		ttl = time.Hour
	}
	var catalog []map[string]any
	for _, typ := range c.types {
		catalog = append(catalog, map[string]any{
			"type": typ,
			"endpoints": []map[string]any{
				{"interface": "public", "region": "RegionOne", "region_id": "RegionOne", "url": c.Endpoint(typ)},
			},
		})
	}
	w.Header().Set("X-Subject-Token", fmt.Sprintf("tok-%d", c.tokens.Add(1)))
	WriteJSON(w, http.StatusCreated, map[string]any{"token": map[string]any{
		"expires_at": time.Now().Add(ttl).UTC().Format("2006-01-02T15:04:05.000000Z"),
		"catalog":    catalog,
		"project": map[string]any{
			"id": "proj-id", "name": "demo-project",
			"domain": map[string]any{"id": "dom-id", "name": "Customers"},
		},
		"user": map[string]any{"id": "user-id", "name": "osmex-user"},
	}})
}

// WriteJSON writes v as a JSON response.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// Test fixtures always encode; a failed write means the client under test hung up.
	_ = json.NewEncoder(w).Encode(v)
}

// Golden compares got with the file at path, rewriting it when -update is set.
func Golden(t *testing.T, path string, got []byte) {
	t.Helper()
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatalf("reading golden file (run with -update to create): %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("output differs from %s:\n--- got\n%s\n--- want\n%s", path, got, want)
	}
}

// Client returns an OpenStack client for the cloud.
func (c *Cloud) Client(t *testing.T) *osclient.Client {
	return osclient.NewClient(osclient.ClientConfig{
		Auth:           c.Auth(t),
		HTTP:           c.Server.Client(),
		Region:         "RegionOne",
		Interface:      "public",
		UserAgent:      "osmex/test",
		MaxConcurrent:  4,
		RequestTimeout: 5 * time.Second,
	})
}
