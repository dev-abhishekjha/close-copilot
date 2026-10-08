package frappe

import "net/http"

const Name = "frappe"

func get(url string) (*http.Response, error) {
	return http.Get(url) // want `http.Get uses the default client`
}

func client() *http.Client {
	return &http.Client{} // want `http.Client literal has no host allowlist`
}

func others() {
	_, _ = http.Head("u")            // want `http.Head uses the default client`
	_, _ = http.Post("u", "", nil)   // want `http.Post uses the default client`
	_, _ = http.PostForm("u", nil)   // want `http.PostForm uses the default client`
	_ = http.DefaultClient           // want `http.DefaultClient reaches any host`
	_ = http.DefaultTransport        // want `http.DefaultTransport reaches any host`
	_ = http.Client{}                // want `http.Client literal has no host allowlist`
	_ = new(http.Client)             // want `new\(http.Client\) has no host allowlist`
	_ = []*http.Client{{Timeout: 1}} // want `http.Client literal has no host allowlist`
	f := http.Get                    // want `http.Get uses the default client`
	_ = f
}

// Using a client someone else built, and building requests, is fine.
func do(c *http.Client, req *http.Request) (*http.Response, error) { return c.Do(req) }

func newReq() (*http.Request, error) { return http.NewRequest(http.MethodGet, "u", nil) }
