// Package httpx is the one place that builds HTTP clients: allowed.
package httpx

import (
	"net/http"
	"net/http/httputil"
)

var zeroClient http.Client
var zeroTransport http.Transport

type EmbedVal struct {
	http.Client
	http.Transport
}

type EmbedPtr struct {
	*http.Client
	*http.Transport
}

type ValueFields struct {
	Client http.Client
	Trans  http.Transport
}

func New() *http.Client {
	_ = &http.Transport{}
	_ = http.Transport{}
	_ = new(http.Transport)
	_ = httputil.NewSingleHostReverseProxy(nil)
	return &http.Client{Transport: http.DefaultTransport}
}
