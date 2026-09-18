package hls

import (
	"encoding/binary"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"
)

// Segment is one playlist entry's bytes and length.
type Segment struct {
	Sequence int
	Duration time.Duration
	Data     []byte
	Started  time.Time
}

// Playlist collects frames into segments and keeps the most recent ones.
type Playlist struct {
	sampleRate      int
	framesPerSeg    int
	window          int
	kept            int
	mu              sync.Mutex
	changed         chan struct{}
	segments        []Segment
	next            int
	framesSeen      int64
	building        []byte
	buildingFrames  int
	buildingStarted time.Time
	origin          time.Time
	now             func() time.Time
}

// NewPlaylist cuts segments of about target, advertises the latest window of
// them, and keeps a few older ones for a client that is slow to fetch.
func NewPlaylist(sampleRate int, target time.Duration, window int, now func() time.Time) *Playlist {
	frames := int(math.Round(target.Seconds() * float64(sampleRate) / SamplesPerFrame))
	return &Playlist{
		sampleRate: sampleRate, framesPerSeg: max(1, frames), window: window, kept: window + 4, changed: make(chan struct{}), now: now,
		// A stream that restarts keeps its URL. Numbering from the clock keeps its
		// sequence rising across the restart, which a player that is still polling
		// needs: a sequence that falls back reads as a playlist that went stale.
		next: int(now().Unix()),
	}
}

func (p *Playlist) frameDuration(frames int) time.Duration {
	return time.Duration(int64(frames) * SamplesPerFrame * int64(time.Second) / int64(p.sampleRate))
}

// timestampTag is the ID3 tag that starts a packed audio segment. It carries
// the segment's first sample as a 33-bit MPEG-2 timestamp at 90 kHz.
func timestampTag(samples int64, sampleRate int) []byte {
	const owner = "com.apple.streaming.transportStreamTimestamp\x00"
	pts := uint64(samples*90000/int64(sampleRate)) & (1<<33 - 1)
	frame := make([]byte, 0, 10+len(owner)+8)
	frame = append(frame, "PRIV"...)
	frame = binary.BigEndian.AppendUint32(frame, uint32(len(owner)+8))
	frame = append(frame, 0, 0)
	frame = append(frame, owner...)
	frame = binary.BigEndian.AppendUint64(frame, pts)

	size := len(frame)
	tag := []byte{'I', 'D', '3', 4, 0, 0, byte(size >> 21 & 0x7F), byte(size >> 14 & 0x7F), byte(size >> 7 & 0x7F), byte(size & 0x7F)}
	return append(tag, frame...)
}

// AddFrame appends one encoded frame, completing a segment when it is due.
func (p *Playlist) AddFrame(frame []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.buildingFrames == 0 {
		p.building = timestampTag(p.framesSeen*SamplesPerFrame, p.sampleRate)
		// Dated from the first frame and the audio since, not from the clock at each
		// cut: the encoder delivers frames in bursts, and a player lines segments up
		// by these dates.
		if p.framesSeen == 0 {
			p.origin = p.now()
		}
		p.buildingStarted = p.origin.Add(p.frameDuration(int(p.framesSeen)))
	}
	p.building = append(p.building, frame...)
	p.buildingFrames++
	p.framesSeen++
	if p.buildingFrames < p.framesPerSeg {
		return
	}
	p.segments = append(p.segments, Segment{Sequence: p.next, Duration: p.frameDuration(p.buildingFrames), Data: p.building, Started: p.buildingStarted})
	p.next++
	p.building, p.buildingFrames = nil, 0
	if extra := len(p.segments) - p.kept; extra > 0 {
		p.segments = p.segments[extra:]
	}
	close(p.changed)
	p.changed = make(chan struct{})
}

// Changed returns a channel that closes when the next segment is complete.
func (p *Playlist) Changed() <-chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.changed
}

// Len is the number of segments a playlist would list now.
func (p *Playlist) Len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return min(len(p.segments), p.window)
}

// Segment returns a segment that is still held.
func (p *Playlist) Segment(sequence int) (Segment, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, segment := range p.segments {
		if segment.Sequence == sequence {
			return segment, true
		}
	}
	return Segment{}, false
}

// Render writes the media playlist. uri names a segment by its sequence.
func (p *Playlist) Render(uri func(sequence int) string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	listed := p.segments[max(0, len(p.segments)-p.window):]
	target := p.frameDuration(p.framesPerSeg)
	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n")
	fmt.Fprintf(&b, "#EXT-X-TARGETDURATION:%d\n", int(math.Ceil(target.Seconds())))
	first := p.next
	if len(listed) > 0 {
		first = listed[0].Sequence
	}
	fmt.Fprintf(&b, "#EXT-X-MEDIA-SEQUENCE:%d\n", first)
	for _, segment := range listed {
		fmt.Fprintf(&b, "#EXT-X-PROGRAM-DATE-TIME:%s\n", segment.Started.UTC().Format("2006-01-02T15:04:05.000Z"))
		fmt.Fprintf(&b, "#EXTINF:%.5f,\n%s\n", segment.Duration.Seconds(), uri(segment.Sequence))
	}
	return b.String()
}
