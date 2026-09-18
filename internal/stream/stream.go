package stream

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"rail-announcements-backend/internal/audio"
	"rail-announcements-backend/internal/feed"
	"rail-announcements-backend/internal/hls"
	"rail-announcements-backend/internal/queue"
)

const (
	// mixInterval is how often audio is handed to the encoder. It bounds how
	// late an announcement starts, and is far below a segment's length.
	mixInterval   = 50 * time.Millisecond
	renderTimeout = 60 * time.Second
)

// Renderer produces the audio for one announcement in one zone.
type Renderer interface {
	// Announce returns the audio for the zone's platforms that the
	// announcement names, and what it has to say about each of them.
	Announce(ctx context.Context, zone Zone, a feed.Announcement) (audio.PCM, []string, error)
}

// Feed is where a stream hears its station's announcements.
type Feed interface {
	Subscribe(crs string) *feed.Subscription
}

// Encoder takes the mixed audio. *hls.Encoder is one.
type Encoder interface {
	Write(pcm []byte) (int, error)
}

type Logger interface {
	Infof(format string, args ...any)
	Warnf(format string, args ...any)
}

// Stream is one listener's continuous audio. It plays silence until its queue
// starts an announcement, and the sum of whatever its zones are saying after.
type Stream struct {
	Zone     Zone
	Key      string
	Playlist *hls.Playlist

	// Radio carries the same audio as MP3, for listeners who take it as one
	// endless response. Nothing feeds it until StartRadio is called.
	Radio      *hls.Broadcast
	radio      atomic.Pointer[hls.Encoder]
	startRadio func() (*hls.Encoder, error)
	radioOnce  sync.Once
	radioErr   error

	renderer Renderer
	log      Logger
	now      func() time.Time
	// lastRequest is when a listener last asked for anything, in Unix nanoseconds.
	lastRequest atomic.Int64
}

// StartRadio starts the MP3 encoder the first time anyone wants it. Most
// listeners take HLS, and a second encoder for each stream would be wasted on them.
func (s *Stream) StartRadio() error {
	s.radioOnce.Do(func() {
		encoder, err := s.startRadio()
		if err != nil {
			s.radioErr = err
			return
		}
		go func() {
			for frame := range encoder.Frames {
				s.Radio.Add(frame)
			}
			s.Radio.Close()
		}()
		s.radio.Store(encoder)
	})
	return s.radioErr
}

// Touch records that a listener is still there.
func (s *Stream) Touch() { s.lastRequest.Store(s.now().UnixNano()) }

func (s *Stream) idleFor() time.Duration {
	return s.now().Sub(time.Unix(0, s.lastRequest.Load()))
}

type speaking struct {
	playback *queue.Playback
	pcm      audio.PCM
}

type rendered struct {
	playback *queue.Playback
	pcm      audio.PCM
	notes    []string
	err      error
}

// lanes lists the zones an announcement holds while it speaks. It is empty for
// an announcement that names none of this stream's platforms.
func (s *Stream) lanes(a feed.Announcement) []string {
	var out []string
	for _, platform := range a.Platforms() {
		if platform == nil || *platform == "" {
			continue
		}
		if lane, ok := s.Zone.lane(*platform); ok && !slices.Contains(out, lane) {
			out = append(out, lane)
		}
	}
	return out
}

// mix adds speech into block, which is 16-bit little-endian samples, holding
// at full scale where two zones together would wrap.
func mix(block, speech []byte) {
	for i := 0; i+1 < len(block) && i+1 < len(speech); i += 2 {
		sum := int32(int16(binary.LittleEndian.Uint16(block[i:]))) + int32(int16(binary.LittleEndian.Uint16(speech[i:])))
		binary.LittleEndian.PutUint16(block[i:], uint16(int16(max(math.MinInt16, min(math.MaxInt16, sum)))))
	}
}

// run mixes until ctx ends. Everything that touches the queue happens on this
// goroutine, which is what lets the queue go without locks.
func (s *Stream) run(ctx context.Context, messages <-chan any, encoder Encoder) error {
	var started []*queue.Playback
	renders := make(chan rendered)
	ready := false

	q := queue.New(
		func(p *queue.Playback) { started = append(started, p) },
		s.now,
		// An announcement for other platforms must be weighed, because it can cut
		// short or outdate one of this stream's. It occupies no lane, so it never
		// makes a zone wait.
		s.lanes,
		func(message string) { s.log.Infof("%s", message) },
		func(err error) { s.log.Warnf("Announcement skipped: %v", err) },
	)

	var current []*speaking
	// begin deals with what the queue started during the call that just returned.
	// Finishing one announcement can start another, so it runs until none are left.
	begin := func() {
		for len(started) > 0 {
			playback := started[0]
			started = started[1:]
			if len(s.lanes(playback.Announcement)) == 0 {
				q.Finished(playback, nil)
				continue
			}
			go func() {
				renderCtx, cancel := context.WithTimeout(playback.Context(), renderTimeout)
				defer cancel()
				pcm, notes, err := s.renderer.Announce(renderCtx, s.Zone, playback.Announcement)
				select {
				case renders <- rendered{playback, pcm, notes, err}:
				case <-ctx.Done():
				}
			}()
		}
	}

	ticker := time.NewTicker(mixInterval)
	defer ticker.Stop()
	epoch := s.now()
	written := int64(0)

	for {
		select {
		case <-ctx.Done():
			q.Reset()
			return nil

		case message, open := <-messages:
			if !open {
				return fmt.Errorf("the announcement feed closed")
			}
			switch m := message.(type) {
			case feed.Disconnected:
				ready = false
				q.Reset()
			case *feed.Ready:
				// Repeated whenever the service's health changes, and each one re-baselines it.
				q.Reset()
				ready = m.Healthy
				if ready {
					s.log.Infof("%s is live; announcements start from now", s.Zone.CRS)
				} else {
					s.log.Infof("%s is recovering; announcements are paused until the service catches up", s.Zone.CRS)
				}
			case *feed.Announcement:
				switch {
				case !ready:
					s.log.Infof("Ignoring a %s sent while the service is recovering", queue.Name(m.Type))
				case s.Zone.wants(m.Type):
					q.Push(*m)
				}
			case *feed.Retraction:
				if ready {
					q.Retract(m.EventID, m.Reason)
				}
			case *feed.Revision:
				if ready {
					q.Revise(m.EventID, m.Details)
				}
			}
			begin()

		case result := <-renders:
			for _, note := range result.notes {
				s.log.Infof("%s", note)
			}
			switch {
			case result.playback.Context().Err() != nil:
			case result.err != nil || !result.playback.Valid() || len(result.pcm) == 0:
				q.Finished(result.playback, result.err)
			default:
				current = append(current, &speaking{result.playback, result.pcm})
			}
			begin()

		case <-ticker.C:
			due := int64(s.now().Sub(epoch))*audio.SampleRate/int64(time.Second) - written
			if due <= 0 {
				continue
			}
			// A stall longer than this is not worth catching up on: the listener's
			// player has already run dry, and a burst would only delay what follows.
			if due > audio.SampleRate {
				written += due - audio.SampleRate
				due = audio.SampleRate
			}
			block := make([]byte, due*2)
			finished := false
			current = slices.DeleteFunc(current, func(voice *speaking) bool {
				if voice.playback.Context().Err() != nil {
					return true
				}
				n := min(len(block), len(voice.pcm))
				mix(block, voice.pcm[:n])
				voice.pcm = voice.pcm[n:]
				if len(voice.pcm) > 0 {
					return false
				}
				q.Finished(voice.playback, nil)
				finished = true
				return true
			})
			if finished {
				begin()
			}
			if _, err := encoder.Write(block); err != nil {
				return fmt.Errorf("encoder: %w", err)
			}
			if radio := s.radio.Load(); radio != nil {
				if _, err := radio.Write(block); err != nil {
					return fmt.Errorf("radio encoder: %w", err)
				}
			}
			written += due
		}
	}
}
