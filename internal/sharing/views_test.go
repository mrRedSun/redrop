package sharing

import (
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/mrRedSun/redrop/internal/store"
	"github.com/mrRedSun/redrop/pkg/api"
)

// TestViewerInfoFields pins what the info button may tell a visitor
// (DESIGN.md: "Info button"). Adding a field here needs a privacy decision.
func TestViewerInfoFields(t *testing.T) {
	var names []string
	typ := reflect.TypeOf(ViewerInfo{})
	for i := 0; i < typ.NumField(); i++ {
		names = append(names, typ.Field(i).Name)
	}
	sort.Strings(names)
	want := []string{"ExpiresAt", "Files", "MaxViews", "Password", "Published", "Size", "Title", "Version", "ViewsLeft"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("ViewerInfo fields changed:\n got %v\nwant %v", names, want)
	}
}

func TestViewerInfoSetting(t *testing.T) {
	on, off := true, false
	for _, tc := range []struct {
		name        string
		kind        string
		setting     *bool
		serverOn    bool
		wantVisible bool
	}{
		{"html follows server default on", api.KindHTML, nil, true, true},
		{"html follows server default off", api.KindHTML, nil, false, false},
		{"explicit on beats server off", api.KindHTML, &on, false, true},
		{"explicit off beats server on", api.KindHTML, &off, true, false},
		{"bundle", api.KindBundle, nil, true, true},
		{"file artifacts never", api.KindFile, &on, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testService()
			s.cfg.InfoButtonDefault = tc.serverOn
			a := &store.Artifact{Kind: tc.kind, InfoButton: tc.setting}
			if _, ok := s.ViewerInfo(a); ok != tc.wantVisible {
				t.Fatalf("visible = %v, want %v", ok, tc.wantVisible)
			}
			if got := s.meta(a, time.Now()).InfoButton; got != (tc.setting == nil && tc.serverOn || tc.setting != nil && *tc.setting) {
				t.Fatalf("meta InfoButton = %v", got)
			}
		})
	}
}

func TestViewerInfoContents(t *testing.T) {
	s := testService()
	s.cfg.InfoButtonDefault = true
	exp := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	pub := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	a := &store.Artifact{
		ID: "secret-id", Slug: "secret-slug", UserID: "u1", OwnerUsername: "owner", TokenName: "agent",
		Kind: api.KindHTML, Title: "Report", CurrentVersion: 3, Published: pub, ExpiresAt: &exp,
		PasswordHash: "x", MaxViews: 5, Views: 2, Size: 1234, Files: 1, TotalSize: 99999, Versions: 3,
	}
	got, ok := s.ViewerInfo(a)
	want := ViewerInfo{Title: "Report", Version: 3, Published: pub, ExpiresAt: &exp, MaxViews: 5, ViewsLeft: 3, Password: true, Size: 1234, Files: 1}
	if !ok || !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	a.MaxViews, a.Views = 0, 40
	if got, _ := s.ViewerInfo(a); got.MaxViews != 0 || got.ViewsLeft != 0 {
		t.Fatalf("unlimited link must not reveal its view count: %+v", got)
	}
	a.MaxViews, a.Views = 1, 1
	if got, _ := s.ViewerInfo(a); got.MaxViews != 1 || got.ViewsLeft != 0 {
		t.Fatalf("one-time link: %+v", got)
	}
}
