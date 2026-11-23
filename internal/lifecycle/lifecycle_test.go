package lifecycle

import (
	"testing"
	"time"
)

func TestScopeAndResourceNames(t *testing.T) {
	a := Scope{RepoID: 1, Env: "preview", PR: 42}
	b := Scope{RepoID: 2, Env: "preview", PR: 42}
	if err := a.Validate(); err != nil || a.LockKey() == b.LockKey() {
		t.Fatal("valid scopes should have distinct lock keys", err)
	}
	if err := (Scope{RepoID: 1, Env: "preview;drop", PR: 42}).Validate(); err == nil {
		t.Fatal("unsafe environment accepted")
	}
	id, err := NewID()
	if err != nil || !ValidID(id) || len(DatabaseName(id)) > 63 || len(RoleName(id)) > 63 {
		t.Fatal("invalid PostgreSQL identifier", err)
	}
}

func TestExpiryProtectsExportedConnections(t *testing.T) {
	cutoff := time.Now()
	for _, tc := range []struct {
		state     State
		old, want bool
	}{
		{Ready, true, true}, {Provisioning, true, true},
		{Deleting, true, true}, {InUse, true, false}, {Ready, false, false},
	} {
		age := cutoff.Add(time.Second)
		if tc.old {
			age = cutoff.Add(-time.Second)
		}
		if got := (Generation{State: tc.state, UpdatedAt: age}).Collectible(cutoff); got != tc.want {
			t.Errorf("state %s, old %t: got %t", tc.state, tc.old, got)
		}
	}
}
