package auth

import (
	"testing"

	"github.com/google/uuid"
)

func TestMasterScopeSeesEveryKey(t *testing.T) {
	s := MasterScope()
	if !s.IsMaster() || !s.Valid() {
		t.Fatal("a master scope must be valid and unrestricted")
	}
	for range 10 {
		if !s.Allows(uuid.New()) {
			t.Error("a master scope must allow any key")
		}
	}
}

func TestKeyScopeSeesOnlyItself(t *testing.T) {
	mine, theirs := uuid.New(), uuid.New()
	s := KeyScope(mine)

	if !s.Allows(mine) {
		t.Error("a key must read its own data")
	}
	if s.Allows(theirs) {
		t.Error("a key must not read another key's data")
	}
	if s.IsMaster() {
		t.Error("a key scope is not a master scope")
	}
}

// A zero Scope is what a handler that forgot to build one would pass. It
// must grant nothing rather than defaulting to something useful.
func TestZeroScopeGrantsNothing(t *testing.T) {
	var s Scope
	if s.Valid() {
		t.Error("the zero scope must not be valid")
	}
	if s.Allows(uuid.New()) || s.Allows(uuid.Nil) {
		t.Error("the zero scope must allow nothing")
	}
	if s.IsMaster() {
		t.Error("the zero scope is not a master scope")
	}
}
