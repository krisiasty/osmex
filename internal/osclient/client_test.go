package osclient_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/krisiasty/osmex/internal/osclient"
	"github.com/krisiasty/osmex/internal/ostest"
)

var (
	computeSpec = osclient.ServiceSpec{Types: []string{"compute"}, Microversion: "compute 2.96"}
	networkSpec = osclient.ServiceSpec{Types: []string{"network"}, Suffix: "/v2.0"}
)

type recorder struct {
	mu    sync.Mutex
	codes []string
}

func (r *recorder) RecordError(code string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.codes = append(r.codes, code)
}

func (r *recorder) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return fmt.Sprint(r.codes)
}

type fakeSleep struct {
	mu    sync.Mutex
	waits []time.Duration
}

func (f *fakeSleep) sleep(ctx context.Context, d time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.waits = append(f.waits, d)
	return nil
}

func newClient(t *testing.T, cloud *ostest.Cloud, maxConcurrent int, sleep func(context.Context, time.Duration) error) *osclient.Client {
	return osclient.NewClient(osclient.ClientConfig{
		Auth:           cloud.Auth(t),
		HTTP:           cloud.Server.Client(),
		Region:         "RegionOne",
		Interface:      "public",
		UserAgent:      "osmex/test",
		MaxConcurrent:  maxConcurrent,
		RequestTimeout: 5 * time.Second,
		Sleep:          sleep,
	})
}

func TestGetSendsHeaders(t *testing.T) {
	cloud := ostest.New(t, "compute", "network")
	cloud.Handle("GET /compute/v2.1/servers", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Auth-Token") != "tok-1" || r.Header.Get("User-Agent") != "osmex/test" ||
			r.Header.Get("OpenStack-API-Version") != "compute 2.96" || r.Header.Get("Accept") != "application/json" {
			t.Errorf("headers = %v", r.Header)
		}
		ostest.WriteJSON(w, 200, map[string]any{"ok": true})
	})
	cloud.Handle("GET /network/v2.0/ports", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := r.Header["Openstack-Api-Version"]; ok {
			t.Errorf("microversion header sent to network")
		}
		ostest.WriteJSON(w, 200, map[string]any{"ok": true})
	})
	c := cloud.Client(t)
	var out struct{ OK bool }
	if err := c.Service(computeSpec).Get(t.Context(), "/servers", nil, &out); err != nil || !out.OK {
		t.Fatalf("compute: %v %v", out, err)
	}
	if err := c.Service(networkSpec).Get(t.Context(), "/ports", nil, &out); err != nil {
		t.Fatalf("network: %v", err)
	}
}

func TestGetNoEndpoint(t *testing.T) {
	cloud := ostest.New(t, "compute")
	rec := &recorder{}
	ctx := osclient.WithRecorder(t.Context(), rec)
	err := cloud.Client(t).Service(osclient.ServiceSpec{Types: []string{"load-balancer"}}).Get(ctx, "/x", nil, nil)
	if osclient.ErrorCode(err) != "catalog" || rec.String() != "[catalog]" {
		t.Errorf("err = %v, recorded %v", err, rec)
	}
}

func TestGet401Reauth(t *testing.T) {
	cloud := ostest.New(t, "compute")
	var calls atomic.Int32
	cloud.Handle("GET /compute/v2.1/x", func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Header.Get("X-Auth-Token") != "tok-2" {
			t.Errorf("retry used token %q", r.Header.Get("X-Auth-Token"))
		}
		ostest.WriteJSON(w, 200, map[string]any{})
	})
	if err := cloud.Client(t).Service(computeSpec).Get(t.Context(), "/x", nil, nil); err != nil {
		t.Fatal(err)
	}
	if cloud.AuthCalls.Load() != 2 {
		t.Errorf("auth calls = %d", cloud.AuthCalls.Load())
	}
}

func TestGet401Twice(t *testing.T) {
	cloud := ostest.New(t, "compute")
	cloud.Handle("GET /compute/v2.1/x", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusUnauthorized) })
	err := cloud.Client(t).Service(computeSpec).Get(t.Context(), "/x", nil, nil)
	var he *osclient.HTTPError
	if !errors.As(err, &he) || he.StatusCode != 401 || cloud.AuthCalls.Load() != 2 {
		t.Errorf("err = %v, auth calls %d", err, cloud.AuthCalls.Load())
	}
}

func TestGet429RetryAfter(t *testing.T) {
	cloud := ostest.New(t, "compute")
	var calls atomic.Int32
	cloud.Handle("GET /compute/v2.1/x", func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) <= 2 {
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		ostest.WriteJSON(w, 200, map[string]any{})
	})
	fs := &fakeSleep{}
	rec := &recorder{}
	ctx := osclient.WithRecorder(t.Context(), rec)
	if err := newClient(t, cloud, 4, fs.sleep).Service(computeSpec).Get(ctx, "/x", nil, nil); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(fs.waits) != "[2s 2s]" || rec.String() != "[429 429]" {
		t.Errorf("waits %v, recorded %v", fs.waits, rec)
	}
}

func TestGet503GivesUp(t *testing.T) {
	cloud := ostest.New(t, "compute")
	var calls atomic.Int32
	cloud.Handle("GET /compute/v2.1/x", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	fs := &fakeSleep{}
	err := newClient(t, cloud, 4, fs.sleep).Service(computeSpec).Get(t.Context(), "/x", nil, nil)
	if osclient.ErrorCode(err) != "503" || calls.Load() != 4 || fmt.Sprint(fs.waits) != "[1s 2s 4s]" {
		t.Errorf("err %v, calls %d, waits %v", err, calls.Load(), fs.waits)
	}
}

func TestGetRetryAfterHTTPDate(t *testing.T) {
	cloud := ostest.New(t, "compute")
	var calls atomic.Int32
	cloud.Handle("GET /compute/v2.1/x", func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", time.Now().Add(3*time.Second).UTC().Format(http.TimeFormat))
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		ostest.WriteJSON(w, 200, map[string]any{})
	})
	fs := &fakeSleep{}
	if err := newClient(t, cloud, 4, fs.sleep).Service(computeSpec).Get(t.Context(), "/x", nil, nil); err != nil {
		t.Fatal(err)
	}
	if len(fs.waits) != 1 || fs.waits[0] < time.Second || fs.waits[0] > 3*time.Second {
		t.Errorf("waits %v", fs.waits)
	}
}

func TestGetRetryPastDeadline(t *testing.T) {
	cloud := ostest.New(t, "compute")
	cloud.Handle("GET /compute/v2.1/x", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	fs := &fakeSleep{}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	err := newClient(t, cloud, 4, fs.sleep).Service(computeSpec).Get(ctx, "/x", nil, nil)
	if osclient.ErrorCode(err) != "429" || len(fs.waits) != 0 {
		t.Errorf("err %v, waits %v", err, fs.waits)
	}
}

func TestGet500NoRetry(t *testing.T) {
	cloud := ostest.New(t, "compute")
	var calls atomic.Int32
	cloud.Handle("GET /compute/v2.1/x", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	err := cloud.Client(t).Service(computeSpec).Get(t.Context(), "/x", nil, nil)
	if osclient.ErrorCode(err) != "500" || calls.Load() != 1 {
		t.Errorf("err %v, calls %d", err, calls.Load())
	}
}

func TestGetDecodeError(t *testing.T) {
	cloud := ostest.New(t, "compute")
	cloud.Handle("GET /compute/v2.1/x", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<html>maintenance</html>"))
	})
	rec := &recorder{}
	ctx := osclient.WithRecorder(t.Context(), rec)
	var out map[string]any
	err := cloud.Client(t).Service(computeSpec).Get(ctx, "/x", nil, &out)
	if osclient.ErrorCode(err) != "decode" || rec.String() != "[decode]" {
		t.Errorf("err %v, recorded %v", err, rec)
	}
}

func TestSemaphore(t *testing.T) {
	cloud := ostest.New(t, "compute")
	var inflight, peak atomic.Int32
	cloud.Handle("GET /compute/v2.1/x", func(w http.ResponseWriter, r *http.Request) {
		n := inflight.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(30 * time.Millisecond)
		inflight.Add(-1)
		ostest.WriteJSON(w, 200, map[string]any{})
	})
	svc := newClient(t, cloud, 2, nil).Service(computeSpec)
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			if err := svc.Get(t.Context(), "/x", nil, nil); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if peak.Load() != 2 {
		t.Errorf("peak in-flight = %d, want 2", peak.Load())
	}
}

func TestGetRefusesPlainHTTPEndpoint(t *testing.T) {
	cloud := ostest.New(t, "compute")
	var plainCalls atomic.Int32
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { plainCalls.Add(1) }))
	defer plain.Close()
	cloud.SetEndpoint("compute", plain.URL+"/v2.1")
	rec := &recorder{}
	err := cloud.Client(t).Service(computeSpec).Get(osclient.WithRecorder(t.Context(), rec), "/servers", nil, nil)
	if osclient.ErrorCode(err) != "insecure" || rec.String() != "[insecure]" {
		t.Errorf("err %v, recorded %v", err, rec)
	}
	if plainCalls.Load() != 0 {
		t.Error("the token was sent to a plain HTTP endpoint")
	}
}

func TestGetAllowsPlainHTTPEndpointWhenInsecure(t *testing.T) {
	cloud := ostest.New(t, "compute")
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ostest.WriteJSON(w, 200, map[string]any{})
	}))
	defer plain.Close()
	cloud.SetEndpoint("compute", plain.URL+"/v2.1")
	c := osclient.NewClient(osclient.ClientConfig{
		Auth: cloud.Auth(t), HTTP: cloud.Server.Client(), Region: "RegionOne", Interface: "public",
		MaxConcurrent: 4, RequestTimeout: 5 * time.Second, AllowInsecure: true,
	})
	if err := c.Service(computeSpec).Get(t.Context(), "/servers", nil, nil); err != nil {
		t.Fatal(err)
	}
}
