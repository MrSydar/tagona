package server

import (
	"mime"
	"strings"
	"unicode/utf8"
)

// Metadata keys that describe how an object is downloaded, when its uploader set them.
const (
	metaContentType = "content_type"
	metaName        = "name"
)

// downloadHeaders returns the Content-Type and Content-Disposition the object store should answer a download with,
// from the object's metadata; each is empty when the object has nothing usable. Both values come from whoever
// uploaded the object, so neither is used unless it is safe in a header: the type must parse as a media type, the
// name is cleaned into a file name. An object with a type is always an attachment, so a browser saves it and
// never renders it from the object store's origin, whatever type it claims.
func downloadHeaders(metadata map[string]string) (contentType, disposition string) {
	if t := metadata[metaContentType]; t != "" && len(t) <= 255 && !strings.ContainsFunc(t, isControl) {
		if _, _, err := mime.ParseMediaType(t); err == nil {
			contentType = t
		}
	}
	name := fileName(metadata[metaName])
	switch {
	case name != "":
		// FormatMediaType quotes or encodes (RFC 2231) as needed; it returns "" only for an invalid name.
		disposition = mime.FormatMediaType("attachment", map[string]string{"filename": name})
	case contentType != "":
		disposition = "attachment"
	}
	return contentType, disposition
}

func isControl(r rune) bool { return r < 0x20 || r == 0x7f }

// fileName makes a stored name usable as a file name: no control characters, no directories, no leading or trailing
// dots, at most 200 bytes. It is empty when nothing is left.
func fileName(stored string) string {
	name := strings.Map(func(r rune) rune {
		switch {
		case isControl(r):
			return -1
		case r == '/' || r == '\\':
			return '_'
		}
		return r
	}, stored)
	name = strings.Trim(strings.TrimSpace(name), ".")
	if len(name) > 200 { // at a character boundary
		cut := 200
		for cut > 0 && !utf8.RuneStart(name[cut]) {
			cut--
		}
		name = name[:cut]
	}
	return name
}
