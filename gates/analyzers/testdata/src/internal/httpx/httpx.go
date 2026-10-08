// Package httpx is the one place that builds HTTP clients: allowed.
package httpx

import "net/http"

func New() *http.Client {
	return &http.Client{Transport: http.DefaultTransport}
}
