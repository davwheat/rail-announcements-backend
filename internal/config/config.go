// Package config loads the service's configuration with Viper and validates it
// with go-playground/validator. Keys in config.toml map to the struct fields
// case-insensitively, and environment variables override them: API.Port is
// API_PORT.
package config

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-playground/validator/v10"
	"github.com/go-viper/mapstructure/v2"
	"github.com/spf13/viper"
)

type Config struct {
	EnableDebugLogs bool

	API    APIConfig
	Feed   FeedConfig
	Audio  AudioConfig
	Stream StreamConfig
}

type APIConfig struct {
	Port int `validate:"min=1,max=65535"`
	// AllowedOrigins are the websites that may play streams and post states. "*" allows any.
	AllowedOrigins []string `validate:"min=1"`
}

type FeedConfig struct {
	// DarwinBrowserURL is the Darwin Browser instance whose announcement streams this service speaks.
	DarwinBrowserURL string `validate:"required,url"`
}

type AudioConfig struct {
	// Directory is the website's audio directory: the one that holds station/ketech.
	Directory string `validate:"required"`
	FFmpeg    string `validate:"required"`
	// CacheMB bounds the decoded clips held in memory.
	CacheMB int `validate:"min=16"`
}

type StreamConfig struct {
	SegmentDuration time.Duration `validate:"min=1s,max=10s"`
	PlaylistWindow  int           `validate:"min=3,max=30"`
	BitrateKbps     int           `validate:"min=24,max=256"`
	IdleTimeout     time.Duration `validate:"min=10s"`
	MaxStreams      int           `validate:"min=1"`
}

func Load(path string) (*Config, error) {
	v := viper.New()
	if path != "" {
		v.SetConfigFile(path)
	} else {
		v.AddConfigPath("/config")
		v.AddConfigPath(".")
		v.SetConfigName("config")
	}
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_", "-", "_"))

	// A default also registers its key, which is what lets AutomaticEnv override it.
	v.SetDefault("EnableDebugLogs", false)
	v.SetDefault("API.Port", 8090)
	v.SetDefault("API.AllowedOrigins", []string{"*"})
	v.SetDefault("Feed.DarwinBrowserURL", "https://darwinbrowser.com")
	v.SetDefault("Audio.Directory", "rail-announcements/audio")
	v.SetDefault("Audio.FFmpeg", "ffmpeg")
	v.SetDefault("Audio.CacheMB", 256)
	v.SetDefault("Stream.SegmentDuration", "1s")
	v.SetDefault("Stream.PlaylistWindow", 8)
	v.SetDefault("Stream.BitrateKbps", 64)
	v.SetDefault("Stream.IdleTimeout", "45s")
	v.SetDefault("Stream.MaxStreams", 200)
	v.AutomaticEnv()

	err := v.ReadInConfig()
	var notFound viper.ConfigFileNotFoundError
	if err != nil && !errors.As(err, &notFound) {
		return nil, fmt.Errorf("read config: %w", err)
	}

	var cfg Config
	hook := viper.DecodeHook(mapstructure.ComposeDecodeHookFunc(
		mapstructure.StringToTimeDurationHookFunc(),
		mapstructure.StringToSliceHookFunc(","),
	))
	if err := v.Unmarshal(&cfg, hook); err != nil {
		return nil, fmt.Errorf("unmarshal config: %w", err)
	}
	if err := validator.New().Struct(cfg); err != nil {
		return nil, fmt.Errorf("config validation failed: %w", err)
	}
	return &cfg, nil
}
