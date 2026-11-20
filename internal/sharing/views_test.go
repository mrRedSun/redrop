package sharing

import (
	"reflect"
	"sort"
	"testing"

	"github.com/mrRedSun/redrop/pkg/api"
)

func fieldNames(v any) []string {
	var names []string
	typ := reflect.TypeOf(v)
	for i := 0; i < typ.NumField(); i++ {
		names = append(names, typ.Field(i).Name)
	}
	sort.Strings(names)
	return names
}

// TestViewerInfoFields pins what the info panel may tell a visitor
// (DESIGN.md "Info panel"). Adding a field here needs a privacy decision.
func TestViewerInfoFields(t *testing.T) {
	if got, want := fieldNames(ViewerInfo{}), []string{"CanOpen", "Current", "ExpiresAt", "Files", "Latest", "MaxViews", "Password",
		"Published", "Revisions", "Size", "Title", "Version", "ViewsLeft"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ViewerInfo fields changed:\n got %v\nwant %v", got, want)
	}
	// A revision is a number, a time and the owner's note: never sizes,
	// files, kind, uploader, or token.
	if got, want := fieldNames(Revision{}), []string{"Current", "Note", "Number", "Published"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Revision fields changed:\n got %v\nwant %v", got, want)
	}
}

// TestModeFor: the only kind policy. HTML and bundles get the frame when the
// info button is on and are served directly when it is off; files always get
// redrop's own file page; end-to-end encrypted versions of every kind always
// get the decrypting viewer.
func TestModeFor(t *testing.T) {
	for _, tc := range []struct {
		kind      string
		info, e2e bool
		want      Mode
	}{
		{api.KindHTML, true, false, ModeFramed}, {api.KindBundle, true, false, ModeFramed},
		{api.KindHTML, false, false, ModeDirect}, {api.KindBundle, false, false, ModeDirect},
		{api.KindFile, true, false, ModeFilePage}, {api.KindFile, false, false, ModeFilePage},
		{api.KindHTML, true, true, ModeEncrypted}, {api.KindBundle, false, true, ModeEncrypted},
		{api.KindFile, true, true, ModeEncrypted}, {api.KindFile, false, true, ModeEncrypted},
	} {
		if got := modeFor(tc.kind, tc.info, tc.e2e); got != tc.want {
			t.Errorf("modeFor(%s, %v, %v) = %v, want %v", tc.kind, tc.info, tc.e2e, got, tc.want)
		}
	}
}
