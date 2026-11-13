package dashpaths

import (
	"os"
	"strings"
	"testing"
)

// TestSPAKnowsServerPaths fails when the dashboard's route table drifts from
// the paths the server redirects to or mints links for.
func TestSPAKnowsServerPaths(t *testing.T) {
	b, err := os.ReadFile("../../web/src/app/paths.ts")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	for _, p := range []string{Home, Login, Account, AccountLinked, TokensWelcome, Setup, Invite, Reset} {
		if !strings.Contains(src, "'"+p+"'") {
			t.Errorf("web/src/app/paths.ts does not declare %q", p)
		}
	}
}
