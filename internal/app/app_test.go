package app

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/krisiasty/osmex/internal/config"
	"github.com/krisiasty/osmex/internal/ostest"
)

func emptyList(key string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { ostest.WriteJSON(w, 200, map[string]any{key: []any{}}) }
}

func fullCloud(t *testing.T, types ...string) *ostest.Cloud {
	cloud := ostest.New(t, types...)
	cloud.Handle("GET /compute/v2.1/servers/detail", func(w http.ResponseWriter, r *http.Request) {
		ostest.WriteJSON(w, 200, map[string]any{"servers": []map[string]any{{"id": "s1", "name": "web", "status": "ACTIVE", "OS-EXT-STS:power_state": 1}}})
	})
	cloud.Handle("GET /compute/v2.1/servers/{id}/os-instance-actions", emptyList("instanceActions"))
	cloud.Handle("GET /compute/v2.1/limits", func(w http.ResponseWriter, r *http.Request) {
		ostest.WriteJSON(w, 200, map[string]any{"limits": map[string]any{"absolute": map[string]any{"maxTotalCores": 10}}})
	})
	cloud.Handle("GET /block-storage/v3/proj/volumes/detail", emptyList("volumes"))
	cloud.Handle("GET /block-storage/v3/proj/snapshots/detail", emptyList("snapshots"))
	cloud.Handle("GET /block-storage/v3/proj/backups/detail", emptyList("backups"))
	cloud.Handle("GET /block-storage/v3/proj/limits", func(w http.ResponseWriter, r *http.Request) {
		ostest.WriteJSON(w, 200, map[string]any{"limits": map[string]any{"absolute": map[string]any{"maxTotalVolumes": 10}}})
	})
	cloud.Handle("GET /network/v2.0/ports", emptyList("ports"))
	cloud.Handle("GET /network/v2.0/floatingips", emptyList("floatingips"))
	cloud.Handle("GET /network/v2.0/quotas/proj-id/details", func(w http.ResponseWriter, r *http.Request) {
		ostest.WriteJSON(w, 200, map[string]any{"quota": map[string]any{"port": map[string]any{"limit": 100, "used": 1}}})
	})
	cloud.Handle("GET /load-balancer/v2/lbaas/loadbalancers", emptyList("loadbalancers"))
	cloud.Handle("GET /load-balancer/v2/lbaas/listeners", emptyList("listeners"))
	cloud.Handle("GET /load-balancer/v2/lbaas/quotas/proj-id", func(w http.ResponseWriter, r *http.Request) {
		ostest.WriteJSON(w, 200, map[string]any{"quota": map[string]any{"load_balancer": 10, "member": -1}})
	})
	cloud.Handle("GET /key-manager/v1/secrets", emptyList("secrets"))
	cloud.Handle("GET /identity/v3/users/user-id/application_credentials/cred-id", func(w http.ResponseWriter, r *http.Request) {
		ostest.WriteJSON(w, 200, map[string]any{"application_credential": map[string]any{"id": "cred-id", "expires_at": "2026-12-31T00:00:00.000000"}})
	})
	return cloud
}

// cloudEnv returns the environment for an exporter talking to cloud (nil for no cloud).
func cloudEnv(t *testing.T, cloud *ostest.Cloud) map[string]string {
	env := map[string]string{}
	if cloud != nil {
		env = map[string]string{
			"OS_AUTH_URL":                      cloud.AuthURL(),
			"OS_APPLICATION_CREDENTIAL_ID":     "cred-id",
			"OS_APPLICATION_CREDENTIAL_SECRET": "cred-secret",
			"OS_REGION_NAME":                   "RegionOne",
		}
		if cloud.Server.TLS != nil {
			env["OS_CACERT"] = cloud.CACertFile(t)
		}
	}
	return env
}

func localListener(t *testing.T) net.Listener {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return ln
}

// serve runs the exporter until the test ends.
func serve(t *testing.T, cfg config.Config, o Options) {
	t.Helper()
	if o.Logger == nil {
		o.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, cfg, o) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run: %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Error("Run did not return after cancel")
		}
	})
}

// start runs the exporter with args against cloud (nil for no cloud) and returns the metrics URL.
func start(t *testing.T, cloud *ostest.Cloud, args ...string) string {
	t.Helper()
	env := cloudEnv(t, cloud)
	cfg, err := config.Parse(args, func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	ln := localListener(t)
	serve(t, cfg, Options{Version: "test", Listener: ln})
	return "http://" + ln.Addr().String() + "/metrics"
}

func scrape(t *testing.T, url string) string {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// eventually scrapes until the body contains every want string.
func eventually(t *testing.T, url string, want ...string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		body := scrape(t, url)
		missing := ""
		for _, w := range want {
			if !strings.Contains(body, w) {
				missing = w
				break
			}
		}
		if missing == "" {
			return body
		}
		if time.Now().After(deadline) {
			t.Fatalf("missing %q in\n%s", missing, body)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestRunNoServices(t *testing.T) {
	body := eventually(t, start(t, nil), `openstack_exporter_build_info{version="test"`, "go_goroutines ", "go_memstats_heap_alloc_bytes ")
	if strings.Contains(body, "auth_up") || strings.Contains(body, "api_up") {
		t.Errorf("unexpected series:\n%s", body)
	}
}

func TestRunExplicitService(t *testing.T) {
	cloud := fullCloud(t, "compute")
	eventually(t, start(t, cloud, "-nova"),
		`openstack_exporter_api_up{poller="nova-servers"} 1`,
		`openstack_exporter_api_up{poller="nova-limits"} 1`,
		`openstack_server_up{id="s1"} 1`,
		"openstack_exporter_auth_up 1")
}

func TestRunAllSkipsMissing(t *testing.T) {
	cloud := fullCloud(t, "compute", "block-storage", "network")
	body := eventually(t, start(t, cloud, "-all"),
		`openstack_exporter_api_up{poller="nova-servers"} 1`,
		`openstack_exporter_api_up{poller="nova-limits"} 1`,
		`openstack_exporter_api_up{poller="cinder"} 1`,
		`openstack_exporter_api_up{poller="cinder-snapshots"} 1`,
		`openstack_exporter_api_up{poller="cinder-backups"} 1`,
		`openstack_exporter_api_up{poller="cinder-limits"} 1`,
		`openstack_exporter_api_up{poller="neutron-ports"} 1`,
		`openstack_exporter_api_up{poller="neutron-fip"} 1`,
		`openstack_exporter_api_up{poller="neutron-limits"} 1`,
		`openstack_network_quota_limit{resource="ports"} 100`)
	if strings.Contains(body, `poller="octavia"`) || strings.Contains(body, `poller="barbican"`) {
		t.Errorf("pollers for services missing from the catalog:\n%s", body)
	}
}

func TestRunAllEveryService(t *testing.T) {
	cloud := fullCloud(t, "compute", "block-storage", "network", "load-balancer", "key-manager")
	eventually(t, start(t, cloud, "-all"),
		`openstack_exporter_api_up{poller="octavia"} 1`,
		`openstack_exporter_api_up{poller="barbican"} 1`,
		`openstack_exporter_api_up{poller="octavia-limits"} 1`,
		`openstack_lb_quota_limit{resource="load_balancers"} 10`)
}

func TestRunExplicitMissingService(t *testing.T) {
	cloud := fullCloud(t, "compute")
	eventually(t, start(t, cloud, "-barbican"),
		`openstack_exporter_api_up{poller="barbican"} 0`,
		`openstack_exporter_api_errors_total{poller="barbican",code="catalog"}`)
}

func TestRunAuthDown(t *testing.T) {
	cloud := fullCloud(t, "compute")
	cloud.FailAuth.Store(true)
	eventually(t, start(t, cloud, "-nova"),
		"openstack_exporter_auth_up 0",
		`openstack_exporter_api_up{poller="nova-servers"} 0`,
		`openstack_exporter_api_errors_total{poller="nova-servers",code="auth"}`)
}

func TestRunListenError(t *testing.T) {
	cfg, err := config.Parse([]string{"-listen-address=256.0.0.1:1"}, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if err := Run(t.Context(), cfg, Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}); err == nil {
		t.Error("invalid listen address accepted")
	}
}

func TestRunBadCACert(t *testing.T) {
	cfg, err := config.Parse([]string{"-nova"}, func(k string) string {
		return map[string]string{
			"OS_AUTH_URL": "https://x", "OS_APPLICATION_CREDENTIAL_ID": "i", "OS_APPLICATION_CREDENTIAL_SECRET": "s",
			"OS_CACERT": os.DevNull,
		}[k]
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := Run(t.Context(), cfg, Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}); err == nil {
		t.Error("unusable OS_CACERT accepted")
	}
}

func TestRunReturnsWhenServerFails(t *testing.T) {
	cloud := fullCloud(t, "compute")
	env := map[string]string{
		"OS_AUTH_URL": cloud.AuthURL(), "OS_CACERT": cloud.CACertFile(t),
		"OS_APPLICATION_CREDENTIAL_ID": "cred-id", "OS_APPLICATION_CREDENTIAL_SECRET": "cred-secret",
	}
	cfg, err := config.Parse([]string{"-nova"}, func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- Run(t.Context(), cfg, Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Listener: ln})
	}()
	eventually(t, "http://"+ln.Addr().String()+"/metrics", `openstack_exporter_api_up{poller="nova-servers"} 1`)
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Error("Run returned nil after the listener failed")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run hung after the HTTP server failed")
	}
}

func TestRunAllStartsPollersWhenLaterAuthFails(t *testing.T) {
	cloud := fullCloud(t, "compute")
	cloud.TokenTTL = 30 * time.Second // shorter than the token reuse margin: every Token() call re-authenticates
	cloud.FailAfter.Store(1)
	eventually(t, start(t, cloud, "-all"), `openstack_exporter_api_up{poller="nova-servers"}`)
}

type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
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

func TestRunLogsCloud(t *testing.T) {
	cloud := fullCloud(t, "compute")
	env := map[string]string{
		"OS_AUTH_URL": cloud.AuthURL(), "OS_CACERT": cloud.CACertFile(t),
		"OS_APPLICATION_CREDENTIAL_ID": "cred-id", "OS_APPLICATION_CREDENTIAL_SECRET": "cred-secret",
		"OS_REGION_NAME": "RegionOne",
	}
	cfg, err := config.Parse([]string{"-nova"}, func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var logs syncBuffer
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, cfg, Options{Version: "test", Logger: slog.New(slog.NewTextHandler(&logs, nil)), Listener: ln})
	}()
	eventually(t, "http://"+ln.Addr().String()+"/metrics", "openstack_exporter_auth_up 1")
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(logs.String(), "project=demo-project") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	out := logs.String()
	for _, want := range []string{
		"msg=\"connecting to cloud\" auth_url=" + cloud.AuthURL() + " region=RegionOne interface=public",
		"msg=\"connected to cloud\"",
		"project=demo-project", "project_id=proj-id", "domain=Customers", "domain_id=dom-id",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log missing %q:\n%s", want, out)
		}
	}
	for _, secret := range []string{"cred-secret", "cred-id", "tok-"} {
		if strings.Contains(out, secret) {
			t.Errorf("log contains %q:\n%s", secret, out)
		}
	}
}

func TestRedactURL(t *testing.T) {
	for in, want := range map[string]string{ //nolint:gosec // G101: fake credentials in a URL, to prove redactURL strips them
		"https://admin:hunter2@keystone.example:5000/v3": "https://keystone.example:5000/v3",
		"https://keystone.example:5000/v3":               "https://keystone.example:5000/v3",
		"https://keystone.example/v3?token=abc":          "https://keystone.example/v3",
		"::not a url":                                    "(unparsable URL)",
	} {
		if got := redactURL(in); got != want {
			t.Errorf("redactURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRunInsecure(t *testing.T) {
	cloud := ostest.NewInsecure(t, "compute")
	cloud.Handle("GET /compute/v2.1/servers/detail", emptyList("servers"))
	cloud.Handle("GET /compute/v2.1/limits", func(w http.ResponseWriter, r *http.Request) {
		ostest.WriteJSON(w, 200, map[string]any{"limits": map[string]any{"absolute": map[string]any{}}})
	})
	eventually(t, start(t, cloud, "-nova", "-insecure"), `openstack_exporter_api_up{poller="nova-servers"} 1`)
}

func TestServiceModuleNames(t *testing.T) {
	for _, m := range Services {
		var got []string
		for _, p := range m.Build(nil, config.Config{}) {
			got = append(got, p.Name())
		}
		if !slices.Equal(got, m.Modules) {
			t.Errorf("service %s builds modules %v, declares %v", m.Name, got, m.Modules)
		}
	}
}

func TestRunAllModuleBeforeAuth(t *testing.T) {
	cloud := fullCloud(t, "compute")
	cloud.FailAuth.Store(true)
	url := start(t, cloud, "-all")
	for target, want := range map[string]int{url + "?module=nova-servers": 200, url + "?module=bogus": 400} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("%s: code %d, want %d\n%s", target, resp.StatusCode, want, b)
		}
		if want == 200 && !strings.Contains(string(b), "openstack_exporter_auth_up 0") {
			t.Errorf("%s: no auth_up 0 in\n%s", target, b)
		}
	}
}

func TestRunAllExcluded(t *testing.T) {
	cloud := fullCloud(t, "compute", "block-storage", "network", "load-balancer", "key-manager")
	url := start(t, cloud, "-all", "-no-barbican", "-no-cinder")
	body := eventually(t, url,
		`openstack_exporter_api_up{poller="nova-servers"} 1`,
		`openstack_exporter_api_up{poller="neutron-ports"} 1`,
		`openstack_exporter_api_up{poller="octavia"} 1`)
	if strings.Contains(body, `poller="barbican"`) || strings.Contains(body, `poller="cinder`) {
		t.Errorf("modules of excluded services:\n%s", body)
	}
	for target, want := range map[string]int{url + "?module=octavia": 200, url + "?module=barbican": 400, url + "?module=cinder-limits": 400} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("%s: code %d, want %d", target, resp.StatusCode, want)
		}
	}
}

func TestRunCredentialExpiry(t *testing.T) {
	cloud := fullCloud(t, "identity", "compute")
	url := start(t, cloud, "-nova")
	eventually(t, url,
		`openstack_exporter_api_up{poller="keystone-credential"} 1`,
		"openstack_exporter_credential_expiration_timestamp_seconds 1.7986752e+09",
		`openstack_exporter_api_up{poller="nova-servers"} 1`)
	eventually(t, url+"?module=keystone-credential", "openstack_exporter_credential_expiration_timestamp_seconds ")
}

func TestRunCredentialFailureIsolated(t *testing.T) {
	// Without an identity endpoint the credential poller fails on its own; other modules are unaffected.
	cloud := fullCloud(t, "compute")
	body := eventually(t, start(t, cloud, "-nova"),
		`openstack_exporter_api_up{poller="keystone-credential"} 0`,
		`openstack_exporter_api_errors_total{poller="keystone-credential",code="catalog"} 1`,
		`openstack_exporter_api_up{poller="nova-servers"} 1`,
		`openstack_server_up{id="s1"} 1`)
	if strings.Contains(body, "credential_expiration") {
		t.Errorf("expiry without a credential read:\n%s", body)
	}
}

func TestRunDebugLogsExchanges(t *testing.T) {
	cloud := fullCloud(t, "compute")
	env := map[string]string{
		"OS_AUTH_URL": cloud.AuthURL(), "OS_CACERT": cloud.CACertFile(t),
		"OS_APPLICATION_CREDENTIAL_ID": "cred-id", "OS_APPLICATION_CREDENTIAL_SECRET": "cred-secret",
		"OS_REGION_NAME": "RegionOne",
	}
	run := func(args ...string) string {
		cfg, err := config.Parse(args, func(k string) string { return env[k] })
		if err != nil {
			t.Fatal(err)
		}
		ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		var logs syncBuffer
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
		go func() { done <- Run(ctx, cfg, Options{Version: "test", Logger: logger, Listener: ln}) }()
		eventually(t, "http://"+ln.Addr().String()+"/metrics", `openstack_exporter_api_up{poller="nova-servers"} 1`)
		cancel()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		return logs.String()
	}

	logs := run("-nova", "-debug")
	for _, want := range []string{`msg="http request"`, `msg="http response"`, "poller=nova-servers", "/compute/v2.1/servers/detail", "/identity/v3/auth/tokens"} {
		if !strings.Contains(logs, want) {
			t.Errorf("-debug: missing %q in:\n%s", want, logs)
		}
	}
	for _, leak := range []string{"cred-secret", "tok-"} {
		if strings.Contains(logs, leak) {
			t.Errorf("-debug: log contains %q", leak)
		}
	}
	// A debug-level logger alone, without -debug, logs no exchanges.
	if logs := run("-nova"); strings.Contains(logs, `msg="http request"`) {
		t.Errorf("exchanges logged without -debug:\n%s", logs)
	}
}

// status returns the response code for url, failing the test if the request fails.
func status(t *testing.T, c *http.Client, url string, user, pass string) int {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if user != "" {
		req.SetBasicAuth(user, pass)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

// eventuallyStatus requests url until it answers with want.
func eventuallyStatus(t *testing.T, url string, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for got := status(t, http.DefaultClient, url, "", ""); got != want; got = status(t, http.DefaultClient, url, "", "") {
		if time.Now().After(deadline) {
			t.Fatalf("%s: code %d, want %d", url, got, want)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestRunTLSAndAuth(t *testing.T) {
	dir := t.TempDir()
	cert, key := ostest.WriteCert(t, dir, "server")
	pwFile := filepath.Join(dir, "pw")
	if err := os.WriteFile(pwFile, []byte("scrape-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Parse([]string{"-listen-tls-cert-file=" + cert, "-listen-tls-key-file=" + key,
		"-listen-auth-username=prometheus", "-listen-auth-password-file=" + pwFile}, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	ln := localListener(t)
	var logs syncBuffer
	serve(t, cfg, Options{Listener: ln, Logger: slog.New(slog.NewTextHandler(&logs, nil))})

	pem, err := os.ReadFile(filepath.Clean(cert))
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(pem)
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}
	base := "https://" + ln.Addr().String()
	for _, c := range []struct {
		path, user, pass string
		want             int
	}{
		{"/metrics", "prometheus", "scrape-secret", 200},
		{"/metrics", "", "", 401},
		{"/metrics", "prometheus", "wrong", 401},
		{"/livez", "", "", 200},
		{"/readyz", "", "", 200},
	} {
		if got := status(t, client, base+c.path, c.user, c.pass); got != c.want {
			t.Errorf("%s as %q: code %d, want %d", c.path, c.user, got, c.want)
		}
	}
	if !strings.Contains(logs.String(), "tls=true") || !strings.Contains(logs.String(), "auth=true") {
		t.Errorf("startup log does not report TLS and auth:\n%s", logs.String())
	}
	if strings.Contains(logs.String(), "in clear") {
		t.Errorf("warned about cleartext with TLS on:\n%s", logs.String())
	}
}

func TestRunProbeListener(t *testing.T) {
	pwFile := filepath.Join(t.TempDir(), "pw")
	if err := os.WriteFile(pwFile, []byte("scrape-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Parse([]string{"-probe-address=127.0.0.1:0", "-listen-auth-username=prometheus", "-listen-auth-password-file=" + pwFile},
		func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	ln, probeLn := localListener(t), localListener(t)
	var logs syncBuffer
	serve(t, cfg, Options{Listener: ln, ProbeListener: probeLn, Logger: slog.New(slog.NewTextHandler(&logs, nil))})

	main, probes := "http://"+ln.Addr().String(), "http://"+probeLn.Addr().String()
	for url, want := range map[string]int{
		main + "/metrics":   401,
		main + "/livez":     200,
		probes + "/livez":   200,
		probes + "/readyz":  200,
		probes + "/metrics": 404,
	} {
		if got := status(t, http.DefaultClient, url, "", ""); got != want {
			t.Errorf("%s: code %d, want %d", url, got, want)
		}
	}
	// The probe listener serves nothing else, even with credentials.
	if got := status(t, http.DefaultClient, probes+"/metrics", "prometheus", "scrape-secret"); got != 404 {
		t.Errorf("probe listener /metrics with credentials: code %d, want 404", got)
	}
	if !strings.Contains(logs.String(), "in clear") {
		t.Errorf("no warning about basic authentication without TLS:\n%s", logs.String())
	}
}

func TestRunListenerFileErrors(t *testing.T) {
	dir := t.TempDir()
	cert, key := ostest.WriteCert(t, dir, "server")
	for name, args := range map[string][]string{
		"missing certificate":   {"-listen-tls-cert-file=" + filepath.Join(dir, "none"), "-listen-tls-key-file=" + key},
		"missing client CA":     {"-listen-tls-cert-file=" + cert, "-listen-tls-key-file=" + key, "-listen-tls-client-ca-file=" + filepath.Join(dir, "none")},
		"missing password file": {"-listen-auth-username=u", "-listen-auth-password-file=" + filepath.Join(dir, "none")},
	} {
		cfg, err := config.Parse(args, func(string) string { return "" })
		if err != nil {
			t.Fatal(err)
		}
		if err := Run(t.Context(), cfg, Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Listener: localListener(t)}); err == nil {
			t.Errorf("%s: Run started", name)
		}
	}
}

func TestRunWarnsAboutLooseSecretFiles(t *testing.T) {
	pwFile := filepath.Join(t.TempDir(), "pw")
	if err := os.WriteFile(pwFile, []byte("scrape-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(pwFile, 0o644); err != nil { //nolint:gosec // G302: the loose mode is what is under test
		t.Fatal(err)
	}
	cfg, err := config.Parse([]string{"-listen-auth-username=u", "-listen-auth-password-file=" + pwFile}, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	var logs syncBuffer
	serve(t, cfg, Options{Listener: localListener(t), Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(logs.String(), "serving metrics") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if out := logs.String(); !strings.Contains(out, "readable beyond its owner") || !strings.Contains(out, "mode=0644") || strings.Contains(out, "scrape-secret") {
		t.Errorf("log:\n%s", out)
	}
}

func TestRunReadiness(t *testing.T) {
	t.Run("explicit services", func(t *testing.T) {
		url := strings.TrimSuffix(start(t, fullCloud(t, "compute"), "-nova"), "/metrics")
		eventuallyStatus(t, url+"/readyz", 200)
	})
	t.Run("all, after the services start", func(t *testing.T) {
		url := strings.TrimSuffix(start(t, fullCloud(t, "compute"), "-all"), "/metrics")
		eventuallyStatus(t, url+"/readyz", 200)
		eventually(t, url+"/metrics", `openstack_exporter_api_up{poller="nova-servers"}`)
	})
	t.Run("all, Keystone failing", func(t *testing.T) {
		// Ready all the same, so a scraper that skips endpoints that are not ready still sees auth_up 0.
		cloud := fullCloud(t, "compute")
		cloud.FailAuth.Store(true)
		url := strings.TrimSuffix(start(t, cloud, "-all"), "/metrics")
		eventuallyStatus(t, url+"/readyz", 200)
		eventually(t, url+"/metrics", "openstack_exporter_auth_up 0")
		eventuallyStatus(t, url+"/livez", 200)
	})
}
