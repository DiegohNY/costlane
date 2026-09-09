package api

import (
	"net/http"

	"github.com/DiegohNY/costlane/internal/auth"
)

// requireMaster gates a handler behind the configured master key.
//
// Every failure returns the same 401 with the same message: distinguishing
// "no header" from "wrong key" would tell an attacker which half of the
// problem to work on.
func (s *Server) requireMaster(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := BearerToken(r)
		if !ok || !auth.VerifyMaster(s.masterKey, token) {
			WriteError(w, http.StatusUnauthorized, ErrorTypeInvalidAPIKey,
				"a valid administrative credential is required in the Authorization header")
			return
		}
		next(w, r)
	}
}
