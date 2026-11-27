package ids

import (
	"regexp"
	"testing"
)

func TestValidID(t *testing.T) {
	idRE := regexp.MustCompile(`^[a-z2-7]{16}$`)
	for range 1000 {
		if id := ID(); !ValidID(id) || !idRE.MatchString(id) {
			t.Fatalf("ID() = %q, which ValidID or the documented form rejects", id)
		}
	}
	for _, s := range []string{
		"", "hoctdvkdnl33xpx", "hoctdvkdnl33xpx3a", "Hoctdvkdnl33xpx3", "hoctdvkdnl33xpx1",
		"hoctdvkdnl33xpx#", "hoctdvkdnl33%23k", "hoctdvkdnl33xpx3%23k=" + "a", "amber-otter-quilt-river", "doesnotexist00",
	} {
		if ValidID(s) || idRE.MatchString(s) != ValidID(s) {
			t.Errorf("ValidID(%q) = true, want false", s)
		}
	}
}
