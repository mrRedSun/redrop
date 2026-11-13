package sharing

import "testing"

func TestCleanBundlePath(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"index.html", "index.html", false},
		{"./assets/app.js", "assets/app.js", false},
		{"a//b/./c.txt", "a/b/c.txt", false},
		{"dir\\file.txt", "dir/file.txt", false},
		{".", "", false},
		{"./", "", false},
		{"../etc/passwd", "", true},
		{"a/../../b", "", true},
		{"/etc/passwd", "", true},
		{"__redrop/open", "", true},
		{"__redrop", "", true},
		{"x\x00y", "", true},
	}
	for _, c := range cases {
		got, err := CleanBundlePath(c.in)
		if (err != nil) != c.wantErr || got != c.want {
			t.Errorf("CleanBundlePath(%q) = %q, %v; want %q, err=%v", c.in, got, err, c.want, c.wantErr)
		}
	}
}

func TestSanitizeFilename(t *testing.T) {
	cases := map[string]string{
		"report.pdf":        "report.pdf",
		"../../etc/passwd":  "passwd",
		"C:\\Users\\x\\a.b": "a.b",
		"":                  "file",
		"..":                "file",
		"__redrop":          "file",
		"bad\"name\n.txt":   "badname.txt",
	}
	for in, want := range cases {
		if got := sanitizeFilename(in); got != want {
			t.Errorf("sanitizeFilename(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHTMLTitle(t *testing.T) {
	if got := htmlTitle([]byte("<html><head><TITLE> A &amp;\n B </TITLE>")); got != "A & B" {
		t.Errorf("htmlTitle = %q", got)
	}
	if got := htmlTitle([]byte("<p>no title</p>")); got != "" {
		t.Errorf("htmlTitle without title = %q", got)
	}
}

func TestVisitorClassification(t *testing.T) {
	bot := Visitor{UserAgent: "TelegramBot (like TwitterBot)"}
	if !bot.IsLinkPreview() || bot.Label() != "Telegram link preview" {
		t.Errorf("telegram bot misclassified: %v %q", bot.IsLinkPreview(), bot.Label())
	}
	human := Visitor{UserAgent: "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0 Safari/537.36"}
	if human.IsLinkPreview() || human.Label() != "Chrome on macOS" {
		t.Errorf("chrome misclassified: %v %q", human.IsLinkPreview(), human.Label())
	}
}
