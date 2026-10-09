package frappe

import (
	"net/http"
	"net/http/httputil"
)

const Name = "frappe"

var zeroClient http.Client       // want `var zeroClient http.Client has no host allowlist`
var zeroTransport http.Transport // want `var zeroTransport http.Transport has no host allowlist`

type EmbedClient struct {
	http.Client // want `struct embeds http.Client`
}

type EmbedClientPtr struct {
	*http.Client // want `struct embeds http.Client`
}

type EmbedTransport struct {
	http.Transport // want `struct embeds http.Transport`
}

type ValueStruct struct {
	Client http.Client    // want `struct field Client http.Client held by value has no host allowlist`
	Trans  http.Transport // want `struct field Trans http.Transport held by value has no host allowlist`
}

func get(url string) (*http.Response, error) {
	return http.Get(url) // want `http.Get uses the default client`
}

func client() *http.Client {
	return &http.Client{} // want `http.Client literal has no host allowlist`
}

func others() {
	_, _ = http.Head("u")                       // want `http.Head uses the default client`
	_, _ = http.Post("u", "", nil)              // want `http.Post uses the default client`
	_, _ = http.PostForm("u", nil)              // want `http.PostForm uses the default client`
	_ = http.DefaultClient                      // want `http.DefaultClient reaches any host`
	_ = http.DefaultTransport                   // want `http.DefaultTransport reaches any host`
	_ = http.Client{}                           // want `http.Client literal has no host allowlist`
	_ = new(http.Client)                        // want `new\(http.Client\) has no host allowlist`
	_ = []*http.Client{{Timeout: 1}}            // want `http.Client literal has no host allowlist`
	f := http.Get                               // want `http.Get uses the default client`
	_ = f
	_ = &http.Transport{}                       // want `http.Transport literal has no host allowlist`
	_ = http.Transport{}                        // want `http.Transport literal has no host allowlist`
	_ = new(http.Transport)                     // want `new\(http.Transport\) has no host allowlist`
	_ = httputil.NewSingleHostReverseProxy(nil) // want `httputil.NewSingleHostReverseProxy reaches any host`
}

// Using a client someone else built, and building requests, is fine.
func do(c *http.Client, req *http.Request) (*http.Response, error) { return c.Do(req) }

func newReq() (*http.Request, error) { return http.NewRequest(http.MethodGet, "u", nil) }
