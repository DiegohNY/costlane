package auth

import (
	"errors"

	"github.com/google/uuid"
)

// Scope says whose data a caller may read.
//
// It is a required parameter on every read query rather than something a
// handler remembers to apply. A handler that forgets to filter does not
// compile, which is the same reason credentials are a Secret rather than a
// string: the compiler enforcing what discipline otherwise has to.
type Scope struct {
	// master grants access to every key's data.
	master bool
	// keyID limits access to one key when master is false.
	keyID uuid.UUID
}

// ErrNoScope reports a scope that was never constructed. A zero Scope grants
// nothing, so forgetting to build one fails closed.
var ErrNoScope = errors.New("auth: request carries no scope")

// MasterScope sees everything.
func MasterScope() Scope { return Scope{master: true} }

// KeyScope sees only the given key's own data.
func KeyScope(keyID uuid.UUID) Scope { return Scope{keyID: keyID} }

// IsMaster reports whether this scope is unrestricted.
func (s Scope) IsMaster() bool { return s.master }

// KeyID returns the key a non-master scope is limited to.
func (s Scope) KeyID() uuid.UUID { return s.keyID }

// Valid reports whether the scope was constructed. The zero value is not.
func (s Scope) Valid() bool { return s.master || s.keyID != uuid.Nil }

// Allows reports whether this scope may read the given key's data.
func (s Scope) Allows(keyID uuid.UUID) bool {
	if !s.Valid() {
		return false
	}
	return s.master || s.keyID == keyID
}
