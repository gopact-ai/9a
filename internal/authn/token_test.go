package authn

import (
	"strings"
	"testing"
)

func TestNewTokenReturnsUniqueNineaTokens(t *testing.T) {
	t.Parallel()
	first, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(first, "ninea_") || !strings.HasPrefix(second, "ninea_") {
		t.Fatalf("NewToken() returned %q and %q", first, second)
	}
	if first == second {
		t.Fatalf("NewToken() returned duplicate token %q", first)
	}
}
