package call

import (
	"strings"
	"sync"
	"testing"
)

func TestTransitions(t *testing.T) {
	t.Parallel()
	allowed := []State{Created, Submitted, Working, InputRequired, Working, Completed}
	for i := 0; i < len(allowed)-1; i++ {
		if !CanTransition(allowed[i], allowed[i+1]) {
			t.Fatalf("expected %s -> %s", allowed[i], allowed[i+1])
		}
	}
	for _, terminal := range []State{Completed, Failed, Canceled, Rejected} {
		if CanTransition(terminal, Working) {
			t.Fatalf("terminal %s transitioned to working", terminal)
		}
	}
}

func TestCallValidate(t *testing.T) {
	t.Parallel()
	if err := (Call{ID: "call-1", CapabilityID: "mcp/weather/get-weather", IdentityID: "agent", State: Created}).Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if err := (Call{ID: "../escape", CapabilityID: "x", State: Created}).Validate(); err == nil {
		t.Fatal("Validate() accepted unsafe call ID")
	}
	for name, candidate := range map[string]Call{
		"unknown state": {ID: "call-1", CapabilityID: "x", IdentityID: "agent", State: "invented"},
		"unsafe code":   {ID: "call-1", CapabilityID: "x", IdentityID: "agent", State: Failed, Code: "bad\ncode"},
		"control text":  {ID: "call-1", CapabilityID: "x", IdentityID: "agent", State: Failed, Code: "failed", Message: "hidden\x00text"},
		"invalid utf8":  {ID: "call-1", CapabilityID: "x", IdentityID: "agent", State: Failed, Code: "failed", Message: string([]byte{0xff})},
		"long code":     {ID: "call-1", CapabilityID: "x", IdentityID: "agent", State: Failed, Code: strings.Repeat("x", 129)},
		"long message":  {ID: "call-1", CapabilityID: "x", IdentityID: "agent", State: Failed, Code: "failed", Message: strings.Repeat("x", 1025)},
	} {
		if err := candidate.Validate(); err == nil {
			t.Errorf("Validate() accepted %s", name)
		}
	}
}

func TestNewIDIsSafeAndUnique(t *testing.T) {
	t.Parallel()
	const count = 256
	ids := make(chan string, count)
	var wg sync.WaitGroup
	for range count {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, err := NewID()
			if err != nil {
				t.Errorf("NewID: %v", err)
				return
			}
			ids <- id
		}()
	}
	wg.Wait()
	close(ids)
	seen := map[string]bool{}
	for id := range ids {
		if !safeID.MatchString(id) {
			t.Fatalf("unsafe ID %q", id)
		}
		if seen[id] {
			t.Fatalf("duplicate ID %q", id)
		}
		seen[id] = true
	}
}
