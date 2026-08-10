package server

//https://github.com/Arcanemagus/plex-api/wiki/Plex-Web-API-Overview

import (
	"encoding/xml"
	"fmt"
	"io/ioutil"
	"net/http"
	urlpkg "net/url"
	"strconv"
	"time"

	configpkg "github.com/adamrdrew/mosh/config"
	"github.com/adamrdrew/mosh/plex_urls"
	"github.com/adamrdrew/mosh/responses"
)

const libraryPageSize = 1000

func GetServer(config *configpkg.Config) Server {
	server := Server{
		Config:   config,
		PlexURLs: plex_urls.GetPlexURLs(config),
	}
	// Interactive Mosh discovers the server through plex.tv. Container/web
	// deployments supply PLEX_BASE_URL instead, so avoid an unnecessary remote
	// discovery request and retain the configured LAN endpoint.
	if config.Address == configpkg.UNINITIALIZED || config.Port == configpkg.UNINITIALIZED {
		server.getServerData()
	}
	return server
}

type Server struct {
	Config   *configpkg.Config
	PlexURLs plex_urls.PlexURLs
}

func (s *Server) panic(err error) {
	if err != nil {
		panic(err.Error())
	}
}

func (s *Server) doGet(urlString string) ([]byte, int) {
	var client = http.Client{Timeout: 15 * time.Second}
	req, err := http.NewRequest("GET", urlString, nil)
	if err != nil {
		return nil, 0
	}

	response, err := client.Do(req)
	if err != nil {
		return nil, 0
	}
	defer response.Body.Close()

	body, err := ioutil.ReadAll(response.Body)
	if err != nil {
		return nil, 0
	}

	return body, response.StatusCode
}

func (s *Server) getServerData() {
	//This URL isn't in PlexURLs because that type provides
	//Plex server queries. This is a plex.tv query. It is a one-off.
	url := "https://plex.tv/pms/servers.xml?X-Plex-Token=" + s.Config.Token

	body, _ := s.doGet(url)

	var serverResponse = new(responses.ServerMediaContainer)
	xmlError := xml.Unmarshal(body, &serverResponse)
	s.panic(xmlError)

	s.Config.Address = serverResponse.Server.Address
	s.Config.Port = serverResponse.Server.Port
	s.Config.Save()
}

func (s *Server) GetLibraries() responses.LibraryMediaContainer {
	url := s.PlexURLs.GetLibraries()
	body, _ := s.doGet(url)

	var serverResponse = new(responses.LibraryMediaContainer)
	xmlError := xml.Unmarshal(body, &serverResponse)
	s.panic(xmlError)

	return *serverResponse
}

func (s *Server) SearchArtists(artistName string) []responses.ResponseArtistDirectory {
	url := s.PlexURLs.SearchArstists(artistName)

	body, respCode := s.doGet(url)
	if respCode != 200 {
		return []responses.ResponseArtistDirectory{}
	}

	var serverResponse = new(responses.ResponseArtistMediaContainer)

	xmlError := xml.Unmarshal(body, &serverResponse)
	s.panic(xmlError)

	return serverResponse.Directories
}

func (s *Server) GetArt(path string) string {
	url := s.PlexURLs.GetArt(path)

	body, respCode := s.doGet(url)
	if respCode != 200 {
		return ""
	}

	return string(body)
}

func (s *Server) SearchAlbums(albumName string) []responses.ResponseAlbumDirectory {
	url := s.PlexURLs.SearchAlbums(albumName)
	body, respCode := s.doGet(url)
	if respCode != 200 {
		return []responses.ResponseAlbumDirectory{}
	}

	var serverResponse = new(responses.ResponseAlbumMediaContainer)
	xmlError := xml.Unmarshal(body, &serverResponse)
	s.panic(xmlError)

	return serverResponse.Directories
}

// SearchTracks mirrors Mosh's artist/album helpers but returns tracks for the
// web API. It is a typed Plex library query, never shell command execution.
func (s *Server) SearchTracks(trackName string) []responses.ResponseTrack {
	url := s.PlexURLs.Server() + "/library/sections/" + s.Config.Library + "/all?type=10&title=" + urlpkg.QueryEscape(trackName) + "&X-Plex-Container-Start=0&X-Plex-Container-Size=50&X-Plex-Token=" + s.Config.Token
	body, respCode := s.doGet(url)
	if respCode != 200 {
		return []responses.ResponseTrack{}
	}

	var response = new(responses.ResponseTracksMediaContainer)
	if err := xml.Unmarshal(body, &response); err != nil {
		return []responses.ResponseTrack{}
	}
	return response.Tracks
}

// GetAllArtists, GetAllAlbums, and GetAllTracks load each collection in
// bounded Plex pages. The web API keeps the result in its own cache, so this
// work happens at startup or an explicit refresh instead of during browsing.
func (s *Server) GetAllArtists() ([]responses.ResponseArtistDirectory, error) {
	items := make([]responses.ResponseArtistDirectory, 0)
	for start := 0; ; {
		url := s.libraryAllURL(8, start)
		body, status := s.doGet(url)
		if status != http.StatusOK {
			return nil, fmt.Errorf("Plex artist library request returned %d", status)
		}
		var response responses.ResponseArtistMediaContainer
		if err := xml.Unmarshal(body, &response); err != nil {
			return nil, err
		}
		items = append(items, response.Directories...)
		if len(response.Directories) < libraryPageSize {
			return items, nil
		}
		start += len(response.Directories)
	}
}

func (s *Server) GetAllAlbums() ([]responses.ResponseAlbumDirectory, error) {
	items := make([]responses.ResponseAlbumDirectory, 0)
	for start := 0; ; {
		url := s.libraryAllURL(9, start)
		body, status := s.doGet(url)
		if status != http.StatusOK {
			return nil, fmt.Errorf("Plex album library request returned %d", status)
		}
		var response responses.ResponseAlbumMediaContainer
		if err := xml.Unmarshal(body, &response); err != nil {
			return nil, err
		}
		items = append(items, response.Directories...)
		if len(response.Directories) < libraryPageSize {
			return items, nil
		}
		start += len(response.Directories)
	}
}

func (s *Server) GetAllTracks() ([]responses.ResponseTrack, error) {
	items := make([]responses.ResponseTrack, 0)
	for start := 0; ; {
		url := s.libraryAllURL(10, start)
		body, status := s.doGet(url)
		if status != http.StatusOK {
			return nil, fmt.Errorf("Plex track library request returned %d", status)
		}
		var response responses.ResponseTracksMediaContainer
		if err := xml.Unmarshal(body, &response); err != nil {
			return nil, err
		}
		items = append(items, response.Tracks...)
		if len(response.Tracks) < libraryPageSize {
			return items, nil
		}
		start += len(response.Tracks)
	}
}

func (s *Server) libraryAllURL(itemType, start int) string {
	// Blur hashes arrive alongside the ordinary library metadata. Asking Plex
	// for the optional field here keeps the cache refresh to the same paged
	// requests instead of making the browser fetch artwork placeholders later.
	return s.PlexURLs.Server() + "/library/sections/" + s.Config.Library + "/all?type=" + strconv.Itoa(itemType) + "&X-Plex-Container-Start=" + strconv.Itoa(start) + "&X-Plex-Container-Size=" + strconv.Itoa(libraryPageSize) + "&resolveTags=1&includeFields=thumbBlurHash,artBlurHash,titleSort,parentTitleSort,grandparentTitleSort,releasetype&X-Plex-Token=" + s.Config.Token
}

func (s *Server) GetAlbumsForArtist(artistID string) []responses.ResponseAlbumDirectory {
	url := s.PlexURLs.GetChildren(artistID)
	body, respCode := s.doGet(url)
	if respCode != 200 {
		return []responses.ResponseAlbumDirectory{}
	}

	var serverResponse = new(responses.ResponseAlbumMediaContainer)
	xmlError := xml.Unmarshal(body, &serverResponse)
	s.panic(xmlError)

	return serverResponse.Directories
}

func (s *Server) GetSongsForAlbum(albumID string) []responses.ResponseTrack {
	url := s.PlexURLs.GetChildren(albumID)

	body, respCode := s.doGet(url)
	if respCode != 200 {
		return []responses.ResponseTrack{}
	}

	var serverResponse = new(responses.ResponseTracksMediaContainer)
	xmlError := xml.Unmarshal(body, &serverResponse)
	s.panic(xmlError)

	return serverResponse.Tracks
}

// GetTrack retrieves one Plex track by rating key for allowlisted web queue
// actions. The caller supplies an ID, never an arbitrary Plex URL.
func (s *Server) GetTrack(trackID string) (responses.ResponseTrack, bool) {
	url := s.PlexURLs.MakeURL("/library/metadata/" + urlpkg.PathEscape(trackID))
	body, respCode := s.doGet(url)
	if respCode != 200 {
		return responses.ResponseTrack{}, false
	}
	var response = new(responses.ResponseTracksMediaContainer)
	if err := xml.Unmarshal(body, &response); err != nil || len(response.Tracks) == 0 {
		return responses.ResponseTrack{}, false
	}
	return response.Tracks[0], true
}

// GetLoudnessLevels retrieves Plex's precomputed seekprint samples for one
// track. It never reads or analyzes the media file; a missing loudness analysis
// simply returns false so callers can retain a normal progress bar.
func (s *Server) GetLoudnessLevels(trackID string) ([]float64, bool) {
	track, found := s.GetTrack(trackID)
	if !found {
		return nil, false
	}
	streamID := track.AudioStreamID()
	if streamID == "" {
		return nil, false
	}
	url := s.PlexURLs.MakeURL("/library/streams/"+urlpkg.PathEscape(streamID)+"/levels") + "&subsample=128"
	body, status := s.doGet(url)
	if status != http.StatusOK {
		return nil, false
	}
	var response responses.ResponseLoudnessLevelsMediaContainer
	if err := xml.Unmarshal(body, &response); err != nil || len(response.Levels) == 0 {
		return nil, false
	}
	levels := make([]float64, 0, len(response.Levels))
	for _, level := range response.Levels {
		levels = append(levels, level.Value)
	}
	return levels, true
}

func (s *Server) MakeURL(part string) string {
	return s.PlexURLs.MakeURL(part)
}
