package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

const (
	refreshedAccessPrefix = "ya29.refreshed-access-"
	rotatedRefreshPrefix  = "1//rotated-refresh-"
)

// refreshGrant answers a refresh_token grant; the caller holds f.mu.
func (f *fakeGoogle) refreshGrant(w http.ResponseWriter, form url.Values) {
	f.refreshes = append(f.refreshes, form)
	w.Header().Set("Content-Type", "application/json")
	knownRefresh := func(token string) bool {
		if f.currentRefresh != "" {
			return token == f.currentRefresh
		}
		return strings.HasPrefix(token, fakeRefreshToken)
	}
	n := len(f.refreshes)
	switch {
	case f.refreshError != "":
		status := f.refreshStatus
		if status == 0 {
			status = http.StatusBadRequest
		}
		w.WriteHeader(status)
		echoed := echoMarker + " " + form.Encode()
		_ = json.NewEncoder(w).Encode(map[string]string{"error": f.refreshError, "error_description": echoed})
		return
	case form.Get("client_id") != fakeClientID || form.Get("client_secret") != fakeClientSecret || !knownRefresh(form.Get("refresh_token")):
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
		return
	}
	access := fmt.Sprintf("%s%04d", refreshedAccessPrefix, n)
	f.validAccess[access] = true
	reply := map[string]any{"access_token": access, "expires_in": 3599, "token_type": "Bearer"}
	if f.rotateRefresh {
		f.currentRefresh = fmt.Sprintf("%s%04d", rotatedRefreshPrefix, n)
		reply["refresh_token"] = f.currentRefresh
	}
	_ = json.NewEncoder(w).Encode(reply)
}

func (f *fakeGoogle) refreshCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.refreshes)
}

func (f *fakeGoogle) refreshForm(i int) url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.refreshes[i]
}

func (f *fakeGoogle) configure(fn func(*fakeGoogle)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}
