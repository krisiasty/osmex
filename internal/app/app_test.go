package app

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
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

// start runs the exporter with args against cloud (nil for no cloud) and returns the metrics URL.
func start(t *testing.T, cloud *ostest.Cloud, args ...string) string {
	t.Helper()
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
	cfg, err := config.Parse(args, func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, cfg, Options{Version: "test", Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Listener: ln})
	}()
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
