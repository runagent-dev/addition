package oauth

import "net/http"

// adapter holds a provider's token-exchange quirks: request shaping and spotting errors inside
// a 2xx response, never token extraction. A non-2xx status is always an error.
type adapter interface {
	beforeTokenExchange(req *http.Request)
	isErrorBody(body map[string]any) bool
	errorCode(body map[string]any) string
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

func (defaultAdapter) isErrorBody(map[string]any) bool { return false }

func (defaultAdapter) errorCode(body map[string]any) string {
	code, _ := body["error"].(string)
	return code
}

func isSuccess(status int) bool { return status >= 200 && status < 300 }
