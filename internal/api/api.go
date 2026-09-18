// Package api is the service's HTTP surface: the live streams, and the
// endpoint that renders a posted announcement.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"rail-announcements-backend/internal/audio"
	"rail-announcements-backend/internal/stream"
	"rail-announcements-backend/internal/system"
)

const (
	// firstSegments is how many segments a new stream must hold before its
	// playlist is served. Players refuse to start on fewer.
	firstSegments    = 2
	firstSegmentWait = 15 * time.Second
	maxStateBytes    = 1 << 20
	renderTimeout    = 60 * time.Second
)

type Logger interface {
	Warnf(format string, args ...any)
}

type Server struct {
	streams *stream.Manager
	library *audio.Library
	origins []string
	log     Logger
}

func New(streams *stream.Manager, library *audio.Library, origins []string, log Logger) *Server {
	return &Server{streams: streams, library: library, origins: origins, log: log}
}

func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(s.cors)
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "streams": s.streams.Count()})
	})
	r.Get("/v1/streams/live.m3u8", s.playlist)
	r.Get("/v1/streams/live.mp3", s.progressive)
	r.Get("/v1/streams/{key}/{segment}", s.segment)
	r.Get("/v1/systems", s.systems)
	r.Post("/v1/announcements", s.render)
	return r
}

func (s *Server) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		switch {
		case slices.Contains(s.origins, "*"):
			w.Header().Set("Access-Control-Allow-Origin", "*")
		case origin != "" && slices.Contains(s.origins, origin):
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Add("Vary", "Origin")
		}
		if r.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
			w.Header().Set("Access-Control-Max-Age", "86400")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]apiError{"error": {code, message}})
}

// stream finds or starts the stream a request describes. It answers the
// request itself, and returns nil, when there is none to give.
func (s *Server) stream(w http.ResponseWriter, r *http.Request) *stream.Stream {
	zone, err := stream.ParseZone(r.URL.Query())
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_zone", err.Error())
		return nil
	}
	live, err := s.streams.Get(zone)
	if errors.Is(err, stream.ErrTooManyStreams) {
		w.Header().Set("Retry-After", "30")
		writeError(w, http.StatusServiceUnavailable, "too_many_streams", err.Error())
		return nil
	}
	return live
}

// progressive serves the stream as one response that never ends, the way
// internet radio does. A browser keeps such a response playing in a background
// tab with no script running, which a playlist that has to be polled cannot
// promise.
func (s *Server) progressive(w http.ResponseWriter, r *http.Request) {
	live := s.stream(w, r)
	if live == nil {
		return
	}
	if err := live.StartRadio(); err != nil {
		s.log.Warnf("Cannot start the MP3 encoder: %v", err)
		writeError(w, http.StatusInternalServerError, "render_failed", "the stream could not be encoded")
		return
	}
	// A player starts only once it holds a few seconds of audio, and it then stays
	// that far behind, because audio that arrives in real time never lets it catch
	// up. Opening with the recent past costs the same delay without the wait.
	recent, frames, stop := live.Radio.Listen()
	defer stop()

	w.Header().Set("Content-Type", "audio/mpeg")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	controller := http.NewResponseController(w)
	w.Write(recent)
	controller.Flush()
	keepAlive := time.NewTicker(10 * time.Second)
	defer keepAlive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepAlive.C:
			live.Touch()
		case frame, open := <-frames:
			if !open {
				return
			}
			controller.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if _, err := w.Write(frame); err != nil {
				return
			}
			controller.Flush()
		}
	}
}

func (s *Server) playlist(w http.ResponseWriter, r *http.Request) {
	live := s.stream(w, r)
	if live == nil {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), firstSegmentWait)
	defer cancel()
	for live.Playlist.Len() < firstSegments {
		select {
		case <-live.Playlist.Changed():
		case <-ctx.Done():
			w.Header().Set("Retry-After", "2")
			writeError(w, http.StatusServiceUnavailable, "stream_starting", "the stream has no audio yet")
			return
		}
	}

	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-store")
	io.WriteString(w, live.Playlist.Render(func(sequence int) string {
		// Relative to /v1/streams/live.m3u8. The station rides along so that a load
		// balancer can send a segment request to the replica that holds the stream:
		// it routes every stream request by its crs parameter.
		return fmt.Sprintf("%s/%d.aac?crs=%s", live.Key, sequence, live.Zone.CRS)
	}))
}

func (s *Server) segment(w http.ResponseWriter, r *http.Request) {
	live := s.streams.Lookup(chi.URLParam(r, "key"))
	sequence, err := strconv.Atoi(strings.TrimSuffix(chi.URLParam(r, "segment"), ".aac"))
	if live == nil || err != nil {
		writeError(w, http.StatusNotFound, "no_segment", "no such segment")
		return
	}
	live.Touch()
	segment, ok := live.Playlist.Segment(sequence)
	if !ok {
		writeError(w, http.StatusNotFound, "no_segment", "the segment has left the stream")
		return
	}
	w.Header().Set("Content-Type", "audio/aac")
	w.Header().Set("Cache-Control", "public, max-age=60, immutable")
	w.Header().Set("Content-Length", strconv.Itoa(len(segment.Data)))
	w.Write(segment.Data)
}

func (s *Server) systems(w http.ResponseWriter, _ *http.Request) {
	type entry struct {
		ID            string   `json:"id"`
		Name          string   `json:"name"`
		Announcements []string `json:"announcements"`
	}
	out := []entry{}
	for _, sys := range system.All {
		out = append(out, entry{sys.ID(), sys.Name(), sys.Announcements()})
	}
	writeJSON(w, http.StatusOK, map[string]any{"systems": out})
}

// render answers a posted tab state with the MP3 that tab would play, or with
// a JSON error.
func (s *Server) render(w http.ResponseWriter, r *http.Request) {
	var request struct {
		System       string          `json:"system"`
		Announcement string          `json:"announcement"`
		State        json.RawMessage `json:"state"`
	}
	body := http.MaxBytesReader(w, r.Body, maxStateBytes)
	if err := json.NewDecoder(body).Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "the body must be JSON with system, announcement and state")
		return
	}
	sys := system.ByID(request.System)
	if sys == nil {
		writeError(w, http.StatusNotFound, "unknown_system", fmt.Sprintf("no system %q", request.System))
		return
	}
	if !slices.Contains(sys.Announcements(), request.Announcement) {
		writeError(w, http.StatusNotFound, "unknown_announcement", fmt.Sprintf("%s has no announcement %q", sys.ID(), request.Announcement))
		return
	}
	announcement, err := sys.Plan(request.Announcement, request.State)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "invalid_state", err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), renderTimeout)
	defer cancel()
	pcm, err := s.library.Render(ctx, sys.FilePrefix(), announcement)
	switch {
	case errors.Is(err, audio.ErrMissing):
		writeError(w, http.StatusUnprocessableEntity, "missing_audio", err.Error())
		return
	case err == nil && len(pcm) == 0:
		writeError(w, http.StatusUnprocessableEntity, "empty_announcement", "the state describes nothing to say")
		return
	case err == nil:
		var mp3 []byte
		if mp3, err = s.library.EncodeMP3(ctx, pcm); err == nil {
			w.Header().Set("Content-Type", "audio/mpeg")
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Content-Length", strconv.Itoa(len(mp3)))
			w.Write(mp3)
			return
		}
	}
	if r.Context().Err() != nil {
		return
	}
	s.log.Warnf("Rendering %s %s failed: %v", sys.ID(), request.Announcement, err)
	writeError(w, http.StatusInternalServerError, "render_failed", "the announcement could not be rendered")
}
