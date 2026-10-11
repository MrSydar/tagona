package server

import (
	"mime"
	"strings"
	"testing"
)

func TestDownloadHeaders(t *testing.T) {
	for _, tc := range []struct {
		name     string
		metadata map[string]string
		wantType string
		wantDisp string // "" none; otherwise the file name expected in the header
		plain    bool   // a bare "attachment"
	}{
		{name: "nothing", metadata: map[string]string{"other": "x"}},
		{name: "both", metadata: map[string]string{"content_type": "image/png", "name": "cat.png"}, wantType: "image/png", wantDisp: "cat.png"},
		{name: "name only", metadata: map[string]string{"name": "cat.png"}, wantDisp: "cat.png"},
		{name: "type only is still an attachment", metadata: map[string]string{"content_type": "text/html"}, wantType: "text/html", plain: true},
		{name: "type with parameters", metadata: map[string]string{"content_type": "text/plain; charset=utf-8"}, wantType: "text/plain; charset=utf-8", plain: true},
		{name: "bad type is dropped", metadata: map[string]string{"content_type": "not a type", "name": "a.txt"}, wantDisp: "a.txt"},
		{name: "header injection in the type", metadata: map[string]string{"content_type": "text/plain\r\nSet-Cookie: x=1"}},
		{name: "directories in the name", metadata: map[string]string{"name": "../../etc/passwd"}, wantDisp: "_.._etc_passwd"},
		{name: "controls and quotes in the name", metadata: map[string]string{"name": "a\r\n\"b\".txt"}, wantDisp: `a"b".txt`},
		{name: "name of dots", metadata: map[string]string{"name": "..."}},
		{name: "not ascii", metadata: map[string]string{"name": "кіт.png"}, wantDisp: "кіт.png"},
		{name: "long name", metadata: map[string]string{"name": strings.Repeat("я", 300)}, wantDisp: strings.Repeat("я", 100)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ct, disp := downloadHeaders(tc.metadata)
			if ct != tc.wantType {
				t.Errorf("type = %q, want %q", ct, tc.wantType)
			}
			if strings.ContainsAny(ct+disp, "\r\n") {
				t.Fatalf("line break in %q %q", ct, disp)
			}
			switch {
			case tc.plain:
				if disp != "attachment" {
					t.Errorf("disposition = %q", disp)
				}
			case tc.wantDisp == "":
				if disp != "" {
					t.Errorf("disposition = %q", disp)
				}
			default:
				kind, params, err := mime.ParseMediaType(disp)
				if err != nil || kind != "attachment" || params["filename"] != tc.wantDisp {
					t.Errorf("disposition = %q (%v, %v)", disp, kind, params)
				}
			}
		})
	}
}

func TestContentRouteType(t *testing.T) {
	for name, tc := range map[string]struct {
		meta map[string]string
		want string
	}{
		"image":       {map[string]string{"content_type": "image/png"}, "image/png"},
		"with params": {map[string]string{"content_type": "text/plain; charset=utf-8"}, "text/plain; charset=utf-8"},
		"none":        {map[string]string{"name": "a.png"}, "application/octet-stream"},
		"not a type":  {map[string]string{"content_type": "nope nope"}, "application/octet-stream"},
		"control":     {map[string]string{"content_type": "image/png\r\nX: y"}, "application/octet-stream"},
		"no metadata": {nil, "application/octet-stream"},
	} {
		if got := contentRouteType(tc.meta); got != tc.want {
			t.Errorf("%s: %q, want %q", name, got, tc.want)
		}
	}
}
