package osclient_test

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/krisiasty/osmex/internal/osclient"
	"github.com/krisiasty/osmex/internal/ostest"
)

type item struct {
	ID string `json:"id"`
}

func TestListAllFollowsLinks(t *testing.T) {
	cloud := ostest.New(t, "compute")
	cloud.Handle("GET /compute/v2.1/servers/detail", func(w http.ResponseWriter, r *http.Request) {
		next := func(marker string) []map[string]string {
			return []map[string]string{{"rel": "next", "href": "http://internal.example:8774/v2.1/servers/detail?limit=2&marker=" + marker}}
		}
		switch r.URL.Query().Get("marker") {
		case "":
			if r.URL.Query().Get("limit") != "1000" {
				t.Errorf("first page limit = %q", r.URL.Query().Get("limit"))
			}
			ostest.WriteJSON(w, 200, map[string]any{"servers": []item{{"1"}, {"2"}}, "servers_links": next("2")})
		case "2":
			ostest.WriteJSON(w, 200, map[string]any{"servers": []item{{"3"}, {"4"}}, "servers_links": next("4")})
		case "4":
			ostest.WriteJSON(w, 200, map[string]any{"servers": []item{{"5"}}})
		}
	})
	got, err := osclient.ListAll[item](t.Context(), cloud.Client(t).Service(computeSpec), "/servers/detail", nil, "servers")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 || got[4].ID != "5" {
		t.Errorf("got %v", got)
	}
}

func TestListAllBarbicanNext(t *testing.T) {
	cloud := ostest.New(t, "key-manager")
	cloud.Handle("GET /key-manager/v1/secrets", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("offset") == "1" {
			ostest.WriteJSON(w, 200, map[string]any{"secrets": []item{{"b"}}, "total": 2, "previous": "http://kms/v1/secrets?limit=1&offset=0"})
			return
		}
		ostest.WriteJSON(w, 200, map[string]any{"secrets": []item{{"a"}}, "total": 2, "next": "http://kms/v1/secrets?limit=1&offset=1"})
	})
	svc := cloud.Client(t).Service(osclient.ServiceSpec{Types: []string{"key-manager"}, Suffix: "/v1"})
	got, err := osclient.ListAll[item](t.Context(), svc, "/secrets", nil, "secrets")
	if err != nil || len(got) != 2 {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestListAllLinksKey(t *testing.T) {
	cloud := ostest.New(t, "compute")
	cloud.Handle("GET /compute/v2.1/servers/s1/os-instance-actions", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("marker") == "" {
			ostest.WriteJSON(w, 200, map[string]any{"instanceActions": []item{{"a"}},
				"links": []map[string]string{{"rel": "next", "href": "http://x/v2.1/servers/s1/os-instance-actions?limit=1&marker=a"}}})
			return
		}
		ostest.WriteJSON(w, 200, map[string]any{"instanceActions": []item{{"b"}}})
	})
	got, err := osclient.ListAll[item](t.Context(), cloud.Client(t).Service(computeSpec), "/servers/s1/os-instance-actions", nil, "instanceActions")
	if err != nil || len(got) != 2 {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestListAllLoop(t *testing.T) {
	cloud := ostest.New(t, "compute")
	cloud.Handle("GET /compute/v2.1/servers/detail", func(w http.ResponseWriter, r *http.Request) {
		ostest.WriteJSON(w, 200, map[string]any{"servers": []item{{"1"}},
			"servers_links": []map[string]string{{"rel": "next", "href": "http://x/v2.1/servers/detail?limit=1000"}}})
	})
	rec := &recorder{}
	_, err := osclient.ListAll[item](osclient.WithRecorder(t.Context(), rec), cloud.Client(t).Service(computeSpec), "/servers/detail", nil, "servers")
	if osclient.ErrorCode(err) != "decode" || rec.String() != "[decode]" {
		t.Errorf("pagination loop: err %v, recorded %v", err, rec)
	}
}

func TestListAllCycle(t *testing.T) {
	// Page A links to B, B to C, and C back to B: no page repeats its predecessor, but the listing never ends.
	cloud := ostest.New(t, "compute")
	var requests atomic.Int32
	cloud.Handle("GET /compute/v2.1/servers/detail", func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		next := map[string]string{"": "b", "b": "c", "c": "b"}[r.URL.Query().Get("marker")]
		ostest.WriteJSON(w, 200, map[string]any{"servers": []item{{"x"}},
			"servers_links": []map[string]string{{"rel": "next", "href": "http://x/v2.1/servers/detail?limit=1000&marker=" + next}}})
	})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	rec := &recorder{}
	_, err := osclient.ListAll[item](osclient.WithRecorder(ctx, rec), cloud.Client(t).Service(computeSpec), "/servers/detail", nil, "servers")
	if osclient.ErrorCode(err) != "decode" || rec.String() != "[decode]" {
		t.Errorf("cycle: err %v (code %q), recorded %v", err, osclient.ErrorCode(err), rec)
	}
	if n := requests.Load(); n != 3 {
		t.Errorf("fetched %d pages, want 3: the cycle must stop at the first repeated page", n)
	}
}

func TestListAllPageLimit(t *testing.T) {
	defer osclient.SetMaxPages(3)()
	cloud := ostest.New(t, "compute")
	var requests atomic.Int32
	cloud.Handle("GET /compute/v2.1/servers/detail", func(w http.ResponseWriter, r *http.Request) {
		n := requests.Add(1) // every page links to a new one
		ostest.WriteJSON(w, 200, map[string]any{"servers": []item{{"x"}},
			"servers_links": []map[string]string{{"rel": "next", "href": fmt.Sprintf("http://x/v2.1/servers/detail?marker=%d", n)}}})
	})
	rec := &recorder{}
	_, err := osclient.ListAll[item](osclient.WithRecorder(t.Context(), rec), cloud.Client(t).Service(computeSpec), "/servers/detail", nil, "servers")
	if osclient.ErrorCode(err) != "decode" || rec.String() != "[decode]" || requests.Load() != 3 {
		t.Errorf("page limit: err %v, recorded %v, pages %d", err, rec, requests.Load())
	}
}

func TestListAllEmpty(t *testing.T) {
	cloud := ostest.New(t, "compute")
	cloud.Handle("GET /compute/v2.1/servers/detail", func(w http.ResponseWriter, r *http.Request) {
		ostest.WriteJSON(w, 200, map[string]any{"servers": []item{}})
	})
	got, err := osclient.ListAll[item](t.Context(), cloud.Client(t).Service(computeSpec), "/servers/detail", nil, "servers")
	if err != nil || len(got) != 0 {
		t.Errorf("got %v, %v", got, err)
	}
}

func TestListAllMissingKey(t *testing.T) {
	cloud := ostest.New(t, "compute")
	cloud.Handle("GET /compute/v2.1/servers/detail", func(w http.ResponseWriter, r *http.Request) {
		ostest.WriteJSON(w, 200, map[string]any{"error": "unexpected"})
	})
	_, err := osclient.ListAll[item](t.Context(), cloud.Client(t).Service(computeSpec), "/servers/detail", nil, "servers")
	if osclient.ErrorCode(err) != "decode" {
		t.Errorf("err = %v; a response without the items key must not look like an empty project", err)
	}
}

func TestListAllNullItems(t *testing.T) {
	cloud := ostest.New(t, "compute")
	cloud.Handle("GET /compute/v2.1/servers/detail", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"servers": null}`))
	})
	_, err := osclient.ListAll[item](t.Context(), cloud.Client(t).Service(computeSpec), "/servers/detail", nil, "servers")
	if osclient.ErrorCode(err) != "decode" {
		t.Errorf("err = %v; a null items list must not look like an empty project", err)
	}
}

func TestListAllMalformedLinks(t *testing.T) {
	// Pagination metadata that is present but can't be followed must fail the listing: treating it as the last page
	// would silently drop every later page.
	for name, page := range map[string]string{
		"links not an array":      `{"servers": [{"id": "1"}], "servers_links": {"rel": "next"}}`,
		"href null":               `{"servers": [{"id": "1"}], "servers_links": [{"rel": "next", "href": null}]}`,
		"href a number":           `{"servers": [{"id": "1"}], "servers_links": [{"rel": "next", "href": 2}]}`,
		"href empty":              `{"servers": [{"id": "1"}], "servers_links": [{"rel": "next", "href": ""}]}`,
		"next link without href":  `{"servers": [{"id": "1"}], "links": [{"rel": "next"}]}`,
		"rel a number":            `{"servers": [{"id": "1"}], "servers_links": [{"rel": 1, "href": "http://x/v2.1/servers/detail?marker=1"}]}`,
		"link null":               `{"servers": [{"id": "1"}], "servers_links": [null]}`,
		"rel missing":             `{"servers": [{"id": "1"}], "servers_links": [{"href": "http://x/v2.1/servers/detail?marker=1"}]}`,
		"rel null":                `{"servers": [{"id": "1"}], "servers_links": [{"rel": null, "href": "http://x/v2.1/servers/detail?marker=1"}]}`,
		"rel empty":               `{"servers": [{"id": "1"}], "servers_links": [{"rel": "", "href": "http://x/v2.1/servers/detail?marker=1"}]}`,
		"top-level next a number": `{"servers": [{"id": "1"}], "next": 2}`,
	} {
		t.Run(name, func(t *testing.T) {
			cloud := ostest.New(t, "compute")
			cloud.Handle("GET /compute/v2.1/servers/detail", func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(page))
			})
			rec := &recorder{}
			got, err := osclient.ListAll[item](osclient.WithRecorder(t.Context(), rec), cloud.Client(t).Service(computeSpec), "/servers/detail", nil, "servers")
			if osclient.ErrorCode(err) != "decode" || rec.String() != "[decode]" {
				t.Errorf("got %v, err %v, recorded %v; want a decode error", got, err, rec)
			}
		})
	}
}

func TestListAllLastPageShapes(t *testing.T) {
	for name, page := range map[string]string{
		"no links":            `{"servers": [{"id": "1"}]}`,
		"links null":          `{"servers": [{"id": "1"}], "servers_links": null, "links": null}`,
		"links empty":         `{"servers": [{"id": "1"}], "servers_links": []}`,
		"only a self link":    `{"servers": [{"id": "1"}], "links": [{"rel": "self", "href": null}]}`,
		"top-level next null": `{"servers": [{"id": "1"}], "next": null}`,
	} {
		t.Run(name, func(t *testing.T) {
			cloud := ostest.New(t, "compute")
			cloud.Handle("GET /compute/v2.1/servers/detail", func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(page))
			})
			got, err := osclient.ListAll[item](t.Context(), cloud.Client(t).Service(computeSpec), "/servers/detail", nil, "servers")
			if err != nil || len(got) != 1 {
				t.Errorf("got %v, %v; want the single page", got, err)
			}
		})
	}
}

func TestListAllWithoutLimit(t *testing.T) {
	// An explicitly empty limit sends none, so the service pages at its own maximum; Nova returns down-cell records only
	// for a listing without paging parameters.
	cloud := ostest.New(t, "compute")
	var first atomic.Bool
	cloud.Handle("GET /compute/v2.1/servers/detail", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := r.URL.Query()["limit"]; ok {
			t.Errorf("request carries limit: %s", r.URL.RawQuery)
		}
		if r.URL.Query().Get("marker") == "" {
			first.Store(true)
			ostest.WriteJSON(w, 200, map[string]any{"servers": []item{{"1"}},
				"servers_links": []map[string]string{{"rel": "next", "href": "http://nova/v2.1/servers/detail?marker=1"}}})
			return
		}
		ostest.WriteJSON(w, 200, map[string]any{"servers": []item{{"2"}}})
	})
	got, err := osclient.ListAll[item](t.Context(), cloud.Client(t).Service(computeSpec), "/servers/detail", url.Values{"limit": {""}}, "servers")
	if err != nil || len(got) != 2 || !first.Load() {
		t.Errorf("got %v, %v", got, err)
	}
}
