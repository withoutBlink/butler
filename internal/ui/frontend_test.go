package ui

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// vendorJS is the only browser JavaScript the panel may ship. It is the
// upstream htmx build and the upstream SSE extension, not project code.
var vendorJS = map[string]bool{
	"vendor/htmx.min.js": true,
	"vendor/sse.js":      true,
}

var handlerAttr = regexp.MustCompile(`(?i)\son[a-z]+\s*=`)

// TestNoHandWrittenFrontendJS fails when someone adds a project-owned
// browser script or an inline event handler. Live behavior belongs in
// templates, handlers, and the vendored htmx libraries.
func TestNoHandWrittenFrontendJS(t *testing.T) {
	root := "."
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel := filepath.ToSlash(path)
		switch {
		case strings.HasPrefix(rel, "static/vendor/"):
			name := strings.TrimPrefix(rel, "static/")
			if !vendorJS[name] {
				t.Errorf("unexpected vendored script %s", rel)
			}
		case strings.HasPrefix(rel, "static/") && strings.HasSuffix(rel, ".js"):
			t.Errorf("hand-written browser script %s; use templates and htmx instead", rel)
		case strings.HasPrefix(rel, "templates/") && strings.HasSuffix(rel, ".tmpl"):
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			s := string(b)
			if strings.Contains(strings.ToLower(s), "javascript:") {
				t.Errorf("%s contains a javascript: URL", rel)
			}
			if handlerAttr.MatchString(s) {
				t.Errorf("%s contains an inline event handler", rel)
			}
			if strings.Contains(s, "<script") && !strings.Contains(s, `vendor/htmx.min.js`) && !strings.Contains(s, `vendor/sse.js`) {
				t.Errorf("%s contains a script that is not the htmx include", rel)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
