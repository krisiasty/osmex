package osclient_test

import (
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/krisiasty/osmex/internal/osclient"
)

func TestNewHTTPClientCA(t *testing.T) {
	dir := t.TempDir()
	if _, err := osclient.NewHTTPClient(filepath.Join(dir, "missing.pem")); err == nil {
		t.Error("missing file accepted")
	}
	empty := filepath.Join(dir, "empty.pem")
	if err := os.WriteFile(empty, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := osclient.NewHTTPClient(empty); err == nil {
		t.Error("file without certificates accepted")
	}

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	ca := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := osclient.NewHTTPClient(ca)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Error(err)
	}

	def, err := osclient.NewHTTPClient("")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := def.Do(req); err == nil {
		t.Error("default roots trusted the test certificate")
	}
}
