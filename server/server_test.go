package server

import (
	"strings"
	"testing"

	"github.com/adamrdrew/mosh/config"
	"github.com/adamrdrew/mosh/plex_urls"
)

func TestLibraryAllURLRequestsReleaseType(t *testing.T) {
	conf := &config.Config{Address: "plex", Port: "32400", Library: "1", Token: "test-token"}
	server := Server{Config: conf, PlexURLs: plex_urls.GetPlexURLs(conf)}
	url := server.libraryAllURL(9, 0)
	if !strings.Contains(url, "resolveTags=1") || !strings.Contains(url, "includeFields=thumbBlurHash,artBlurHash,titleSort,parentTitleSort,grandparentTitleSort,releasetype") {
		t.Fatalf("libraryAllURL() = %q; expected resolved formats and releasetype", url)
	}
}

func TestLibraryAllURLWithPlexFilter(t *testing.T) {
	conf := &config.Config{Address: "plex", Port: "32400", Library: "1", Token: "test-token"}
	server := Server{Config: conf, PlexURLs: plex_urls.GetPlexURLs(conf)}
	url := server.libraryAllURLWithFilter(9, 0, "format!=EP%2CSingle&subformat=Soundtrack")
	if !strings.Contains(url, "format!=EP%2CSingle&subformat=Soundtrack") {
		t.Fatalf("libraryAllURLWithFilter() = %q; expected Plex music-format filter", url)
	}
}
