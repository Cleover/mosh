package responses

import (
	"encoding/xml"
	"testing"
)

func TestResponseAlbumDirectoryReadsMusicReleaseType(t *testing.T) {
	var album ResponseAlbumDirectory
	if err := xml.Unmarshal([]byte(`<Directory type="album" title="First Light" releasetype="album;compilation" subtype="album" />`), &album); err != nil {
		t.Fatalf("unmarshal album directory: %v", err)
	}
	if album.ReleaseType != "album;compilation" {
		t.Fatalf("ReleaseType = %q; want %q", album.ReleaseType, "album;compilation")
	}
}
