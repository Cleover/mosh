package responses

import "encoding/xml"

type ResponseAlbumDirectory struct {
	XMLName xml.Name `xml:"Directory"`
	//Will always be album
	Type string `xml:"type,attr"`
	//This is the endpoint we use to interact with this album
	//example /library/metadata/33342/children
	Key string `xml:"key,attr"`
	//Album title
	Title     string `xml:"title,attr"`
	TitleSort string `xml:"titleSort,attr"`
	//This is like the artist's ID. We can use it to reverse engineer
	//the key. This makes running commands easier
	RatingKey string `xml:"ratingKey,attr"`
	//Artist Name
	ParentTitle     string `xml:"parentTitle,attr"`
	ParentTitleSort string `xml:"parentTitleSort,attr"`
	ParentRatingKey string `xml:"parentRatingKey,attr"`
	// ReleaseType is Plex's music-specific release classification, for example
	// "album;compilation" or "single". SubType is a generic media refinement
	// and does not drive Plexamp's Albums / Singles / Soundtracks grouping.
	ReleaseType string `xml:"releasetype,attr"`
	SubType     string `xml:"subtype,attr"`
	Year        int    `xml:"year,attr"`
	//Album artwork, served through the web API's authenticated artwork proxy.
	Thumb         string                `xml:"thumb,attr"`
	ThumbBlurHash string                `xml:"thumbBlurHash,attr"`
	Formats       []ResponseAlbumFormat `xml:"Format"`
}

// ResponseAlbumFormat is included when Plex receives resolveTags=1. Plexamp
// uses these music-format tags (Album, EP, Single, Soundtrack, Compilation)
// to split an artist's discography into its sections.
type ResponseAlbumFormat struct {
	Tag string `xml:"tag,attr"`
}

type ResponseAlbumMediaContainer struct {
	XMLName     xml.Name                 `xml:"MediaContainer"`
	Directories []ResponseAlbumDirectory `xml:"Directory"`
}
