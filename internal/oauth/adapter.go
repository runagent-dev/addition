package oauth

import "net/http"

// adapter holds a provider's token-exchange quirks: request shaping and error detection, never token extraction.
type adapter interface {
	beforeTokenExchange(req *http.Request)
	isError(status int, body map[string]any) bool
	errorDetail(body map[string]any) (code, description string)
}

// adapters is keyed by provider id; Google and unknown providers use the standard behaviour.
var adapters = map[string]adapter{}

func adapterFor(providerID string) adapter {
	if a, ok := adapters[providerID]; ok {
		return a
	}
	return defaultAdapter{}
}

type defaultAdapter struct{}

func (defaultAdapter) beforeTokenExchange(*http.Request) {}

func (defaultAdapter) isError(status int, _ map[string]any) bool { return status >= 400 }

func (defaultAdapter) errorDetail(body map[string]any) (string, string) {
	code, _ := body["error"].(string)
	if code == "" {
		return "unknown_error", ""
	}
	description, _ := body["error_description"].(string)
	return code, description
}
