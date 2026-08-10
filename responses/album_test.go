package responses

import (
	"encoding/xml"
	"testing"
)

func TestResponseAlbumDirectoryReadsMusicReleaseType(t *testing.T) {
	var album ResponseAlbumDirectory
	if err := xml.Unmarshal([]byte(`<Directory type="album" title="First Light" releasetype="album;compilation" subtype="album"><Format tag="Album"/><Format tag="Compilation"/></Directory>`), &album); err != nil {
		t.Fatalf("unmarshal album directory: %v", err)
	}
	if album.ReleaseType != "album;compilation" {
		t.Fatalf("ReleaseType = %q; want %q", album.ReleaseType, "album;compilation")
	}
	if len(album.Formats) != 2 || album.Formats[1].Tag != "Compilation" {
		t.Fatalf("Formats = %#v; want Album and Compilation", album.Formats)
	}
}
