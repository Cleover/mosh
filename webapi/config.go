package webapi

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

type AppConfig struct {
	Addr              string
	AdminSecret       string
	SigningSecret     string
	InternalAPISecret string
	DataPath          string
	FFmpegPath        string
	Bitrate           string
	SecureCookies     bool
}

func LoadConfig() (AppConfig, error) {
	c := AppConfig{
		Addr:              env("WEB_API_ADDR", ":8080"),
		AdminSecret:       os.Getenv("ADMIN_SECRET"),
		SigningSecret:     os.Getenv("SESSION_SIGNING_SECRET"),
		InternalAPISecret: os.Getenv("INTERNAL_API_SECRET"),
		DataPath:          env("WEB_STATE_PATH", "/data/sessions.json"),
		FFmpegPath:        env("FFMPEG_PATH", "ffmpeg"),
		Bitrate:           env("STREAM_BITRATE", "320k"),
		SecureCookies:     strings.EqualFold(env("SECURE_COOKIES", "true"), "true"),
	}
	if len(c.AdminSecret) < 16 {
		return c, errors.New("ADMIN_SECRET must be at least 16 characters")
	}
	if len(c.SigningSecret) < 32 {
		return c, errors.New("SESSION_SIGNING_SECRET must be at least 32 characters")
	}
	if len(c.InternalAPISecret) < 32 {
		return c, errors.New("INTERNAL_API_SECRET must be at least 32 characters")
	}
	if err := os.MkdirAll(filepath.Dir(c.DataPath), 0700); err != nil {
		return c, err
	}
	return c, nil
}

func env(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}
