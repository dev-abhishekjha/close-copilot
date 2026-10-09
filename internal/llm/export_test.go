package llm

import "net/http"

// WithHTTPClient overrides the outbound HTTP client. It exists only in tests,
// so production code always goes through internal/httpx.
func WithHTTPClient(client *http.Client) AnthropicOption {
	return func(c *anthropicConfig) {
		c.httpClient = client
	}
}
