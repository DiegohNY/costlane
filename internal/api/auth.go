// Package api serves the read and administrative HTTP routes.
package api

import (
	"net/http"
	"strings"

	"github.com/DiegohNY/costlane/internal/obs"
)

// credentialParams are query parameter names that carry a credential.
//
// A credential in a query string is recorded by every proxy, browser history
// and access log between the client and here — including our own. One of the
// projects surveyed during design accepted keys this way; refusing them is
// cheap and closes the hole for anyone who copies a URL from a tutorial.
var credentialParams = []string{
	"api_key", "apikey", "key", "token", "access_token", "auth", "authorization",
}

// RejectQueryCredentials refuses any request carrying a credential in its
// query string.
//
// The offending value is never logged nor echoed: repeating it in the
// response body would move the leak into the client's own logs rather than
// preventing it.
func RejectQueryCredentials(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		for name := range query {
			if isCredentialParam(name) {
				WriteError(w, http.StatusBadRequest, ErrorTypeInvalidRequest,
					"credentials must be sent in the Authorization header, "+
						"not in the query string, where proxies and access logs record them")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func isCredentialParam(name string) bool {
	lower := strings.ToLower(name)
	for _, candidate := range credentialParams {
		if lower == candidate {
			return true
		}
	}
	return false
}

// BearerToken extracts the credential from an Authorization header.
//
// The value is returned as a Secret so that from the moment it enters the
// process it cannot reach a log or an error message by accident.
func BearerToken(r *http.Request) (obs.Secret, bool) {
	header := r.Header.Get("Authorization")
	if header == "" {
		return "", false
	}
	scheme, value, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return "", false
	}
	return obs.Secret(value), true
}
