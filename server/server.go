package server

//https://github.com/Arcanemagus/plex-api/wiki/Plex-Web-API-Overview

import (
	"encoding/xml"
	"io/ioutil"
	"net/http"
	urlpkg "net/url"
	"time"

	configpkg "github.com/adamrdrew/mosh/config"
	"github.com/adamrdrew/mosh/plex_urls"
	"github.com/adamrdrew/mosh/responses"
)

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

func (s *Server) MakeURL(part string) string {
	return s.PlexURLs.MakeURL(part)
}
