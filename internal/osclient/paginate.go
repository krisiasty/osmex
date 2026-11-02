package osclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
)

// maxPages bounds one listing: 10 million items at 1000 per page, far beyond any project, so only an API that
// keeps inventing new pages reaches it.
var maxPages = 10000

// ListAll fetches every page of a paginated list and returns the items under itemsKey. It follows
// the rel="next" link in "<itemsKey>_links" or "links", or a top-level "next" URL (Barbican). Only
// the query string of a next link is used, because services often return links with internal host
// names or the wrong scheme. A response without an itemsKey list (missing or null) is a decode error, never an empty list.
// A next link that repeats any page already fetched, malformed pagination links, or a listing longer than maxPages,
// is a decode error too.
// The first request asks for 1000 items unless query sets a limit; an explicitly empty limit sends none, so the service
// pages at its own maximum.
func ListAll[T any](ctx context.Context, s *ServiceClient, path string, query url.Values, itemsKey string) ([]T, error) {
	q := url.Values{}
	for k, v := range query {
		q[k] = append([]string(nil), v...)
	}
	switch limit, ok := q["limit"]; {
	case !ok:
		q.Set("limit", "1000")
	case len(limit) == 0 || limit[0] == "":
		q.Del("limit")
	}
	var all []T
	seen := map[string]bool{q.Encode(): true}
	for pages := 1; ; pages++ {
		var page map[string]json.RawMessage
		if err := s.Get(ctx, path, q, &page); err != nil {
			return nil, err
		}
		raw, ok := page[itemsKey]
		if !ok || string(raw) == "null" {
			err := &DecodeError{Err: fmt.Errorf("response has no %q list", itemsKey)}
			recordError(ctx, err)
			return nil, err
		}
		var items []T
		if err := json.Unmarshal(raw, &items); err != nil {
			err = &DecodeError{Err: err}
			recordError(ctx, err)
			return nil, err
		}
		all = append(all, items...)

		next, err := nextLink(page, itemsKey)
		if err != nil {
			err = &DecodeError{Err: err}
			recordError(ctx, err)
			return nil, err
		}
		if next == "" {
			return all, nil
		}
		nu, err := url.Parse(next)
		if err != nil {
			err = &DecodeError{Err: err}
			recordError(ctx, err)
			return nil, err
		}
		nq := nu.Query()
		switch {
		case seen[nq.Encode()]:
			err := &DecodeError{Err: fmt.Errorf("pagination loop at %s: a next link repeats an earlier page", path)}
			recordError(ctx, err)
			return nil, err
		case pages >= maxPages:
			err := &DecodeError{Err: fmt.Errorf("pagination at %s exceeded %d pages", path, maxPages)}
			recordError(ctx, err)
			return nil, err
		}
		seen[nq.Encode()] = true
		q = nq
	}
}

// nextLink returns the URL of the next page, or "" on the last page. Pagination metadata that is present but can't
// be followed is an error: taking it for the last page would silently drop the rest of the listing.
func nextLink(page map[string]json.RawMessage, itemsKey string) (string, error) {
	for _, key := range []string{itemsKey + "_links", "links"} {
		raw, ok := page[key]
		if !ok || string(raw) == "null" {
			continue
		}
		// Pointers tell a null entry or a missing or null rel, which could be a next link, from a real relation.
		var links []*struct {
			Rel  *string         `json:"rel"`
			Href json.RawMessage `json:"href"`
		}
		if err := json.Unmarshal(raw, &links); err != nil {
			return "", fmt.Errorf("malformed %q: %w", key, err)
		}
		for _, l := range links {
			if l == nil || l.Rel == nil || *l.Rel == "" {
				return "", fmt.Errorf("%q has a link without a relation", key)
			}
			if *l.Rel != "next" {
				continue
			}
			var href string
			if json.Unmarshal(l.Href, &href) != nil || href == "" {
				return "", fmt.Errorf("%q has a next link without an href", key)
			}
			return href, nil
		}
	}
	raw, ok := page["next"]
	if !ok || string(raw) == "null" {
		return "", nil
	}
	var next string
	if err := json.Unmarshal(raw, &next); err != nil {
		return "", fmt.Errorf("malformed \"next\": %w", err)
	}
	return next, nil
}
