// Package web takes its client from httpx: clean.
package web

import (
	"context"
	"net/http"
)

type Fetcher struct{ Client *http.Client }

func (f Fetcher) Fetch(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	return f.Client.Do(req)
}
