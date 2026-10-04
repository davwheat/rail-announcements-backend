package hls

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestPlaylistCutsSegmentsAndSlides(t *testing.T) {
	started := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	playlist := NewPlaylist(44100, 2*time.Second, 3, started)
	// 86 frames of 1024 samples are 1.997 s, the nearest whole number of frames to
	// 2 s, and this many of those segments fit between the Unix epoch and the start.
	const first = 896244764
	frame := []byte{0xFF, 0xF1, 0, 0, 0, 0, 0, 1, 2, 3}

	for range 85 {
		playlist.AddFrame(frame)
	}
	if playlist.Len() != 0 {
		t.Fatal("a segment was cut early")
	}
	changed := playlist.Changed()
	playlist.AddFrame(frame)
	select {
	case <-changed:
	default:
		t.Fatal("completing a segment did not signal")
	}

	for range 86 * 9 {
		playlist.AddFrame(frame)
	}
	rendered := playlist.Render(func(sequence int) string { return fmt.Sprintf("s/%d.aac", sequence) })
	for _, want := range []string{
		"#EXT-X-VERSION:3\n",
		"#EXT-X-TARGETDURATION:2\n",
		"#EXT-X-MEDIA-SEQUENCE:896244771\n",
		// The stream's first sequence number, kept small: 896244764 modulo 65536.
		"#EXT-X-DISCONTINUITY-SEQUENCE:39964\n",
		"#EXTINF:1.99692,\ns/896244771.aac\n",
		"#EXTINF:1.99692,\ns/896244773.aac\n",
		// Segments are dated by their place on the timeline, whatever the clock said when they were cut.
		"#EXT-X-PROGRAM-DATE-TIME:2026-09-18T10:00:12.547Z\n",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("playlist lacks %q:\n%s", want, rendered)
		}
	}
	if strings.Contains(rendered, "ENDLIST") || strings.Count(rendered, "#EXTINF") != 3 {
		t.Errorf("a live playlist lists its window and never ends:\n%s", rendered)
	}

	// A client on a slow connection fetches from a playlist it read a while ago.
	// The oldest playlist that can still be in use is the one that listed the
	// segment before the three listed now, at the end of its window.
	if _, ok := playlist.Segment(first + 3); !ok {
		t.Error("a segment that a slow client can still ask for is gone")
	}
	if _, ok := playlist.Segment(first + 2); ok {
		t.Error("a segment that no playlist in use lists is still held")
	}
	segment, ok := playlist.Segment(first + 9)
	if !ok {
		t.Fatal("the newest segment is missing")
	}
	if !bytes.HasSuffix(segment.Data, frame) || len(segment.Data) <= 86*len(frame) {
		t.Error("the segment is not its tag followed by its frames")
	}
}

// A stream stops when nobody has asked for it for a while, and when its replica
// is replaced. A listener whose connection dropped for that long, or whose
// station moved to the other replica, is still polling the same URL, and its
// player reads the new stream's playlist as the old one's next version.
func TestARestartedStreamCarriesOnTheTimelineOfTheOneBefore(t *testing.T) {
	const hours, segmentFrames = 60, 43
	started := time.Date(2026, 10, 4, 6, 0, 0, 0, time.UTC)
	frame := []byte{0xFF, 0xF1, 0, 0, 0, 0, 0}
	newest := func(p *Playlist) Segment {
		segment, ok := p.Segment(p.next - 1)
		if !ok {
			t.Fatal("the playlist holds no segment")
		}
		return segment
	}
	timestamp := func(s Segment) int64 {
		tag := timestampTag(0, 44100)
		return int64(binary.BigEndian.Uint64(s.Data[len(tag)-8 : len(tag)]))
	}

	// Audio arrives as fast as the clock runs and no faster. A segment is a
	// little under a second, so a stream cuts more segments than seconds pass.
	old := NewPlaylist(44100, time.Second, 8, started)
	for range hours * 3600 * 44100 / SamplesPerFrame {
		old.AddFrame(frame)
	}
	stopped := started.Add(hours * time.Hour)
	last := newest(old)
	if behind := stopped.Sub(last.Started); behind < 0 || behind > 2*time.Second {
		t.Errorf("after %d hours the newest segment is dated %s, which is %s from the clock", hours, last.Started.UTC(), behind)
	}

	restarted := NewPlaylist(44100, time.Second, 8, stopped)
	for range segmentFrames {
		restarted.AddFrame(frame)
	}
	first := newest(restarted)
	missed := int64(first.Sequence - last.Sequence)
	if missed <= 0 {
		t.Fatalf("the restarted stream numbers from %d, and the old one had reached %d: the player reads that as a stale playlist", first.Sequence, last.Sequence)
	}
	// The timestamp and the date are what place a segment's audio in the player.
	// Both have to move on by exactly the segments missed, or the audio lands
	// where the player has already played.
	const samples = segmentFrames * SamplesPerFrame
	if got, want := first.Started.Sub(last.Started), time.Duration(missed*samples*int64(time.Second)/44100); (got - want).Abs() > time.Microsecond {
		t.Errorf("the restarted stream is dated %s after the old one, want %s", got, want)
	}
	if got, want := (timestamp(first)-timestamp(last))&(1<<33-1), missed*samples*90000/44100; got < want-1 || got > want+1 {
		t.Errorf("the restarted stream's timestamp is %d after the old one's, want %d", got, want)
	}

	// The audio on either side of the restart comes from two encoders. A player
	// learns that from a discontinuity sequence number that changed, and plays
	// the new audio where it stopped. Otherwise it plays the segments that it
	// missed as silence, and is that far behind for as long as it listens.
	number := func(p *Playlist) int {
		rendered := p.Render(func(int) string { return "" })
		_, after, _ := strings.Cut(rendered, "#EXT-X-DISCONTINUITY-SEQUENCE:")
		line, _, _ := strings.Cut(after, "\n")
		n, err := strconv.Atoi(line)
		if err != nil {
			t.Fatalf("no discontinuity sequence number in:\n%s", rendered)
		}
		return n
	}
	if before, after := number(old), number(restarted); before == after || max(before, after) >= 1<<16 {
		t.Errorf("discontinuity sequence numbers %d and %d: a restart needs a different one, small enough for hls.js to walk", before, after)
	}
}

func TestSegmentsStartWithTheirTimestamp(t *testing.T) {
	tag := timestampTag(44100*10, 44100)
	if string(tag[:3]) != "ID3" || tag[3] != 4 {
		t.Fatalf("not an ID3v2.4 tag: % x", tag[:10])
	}
	size := int(tag[6])<<21 | int(tag[7])<<14 | int(tag[8])<<7 | int(tag[9])
	if size != len(tag)-10 {
		t.Errorf("tag declares %d bytes and holds %d", size, len(tag)-10)
	}
	if !bytes.Contains(tag, []byte("PRIV")) || !bytes.Contains(tag, []byte("com.apple.streaming.transportStreamTimestamp\x00")) {
		t.Error("the tag lacks the transport stream timestamp frame")
	}
	if pts := binary.BigEndian.Uint64(tag[len(tag)-8:]); pts != 900000 {
		t.Errorf("ten seconds is %d at 90 kHz, want 900000", pts)
	}
	// A segment's place is counted from the Unix epoch. By 2050 that count, times
	// 90,000, is more than an int64 holds.
	const epochSeconds = 2_524_608_000
	epoch := timestampTag(epochSeconds*44100+22050, 44100)
	if pts, want := binary.BigEndian.Uint64(epoch[len(epoch)-8:]), uint64(epochSeconds*90000+45000)&(1<<33-1); pts != want {
		t.Errorf("a timestamp counted from the epoch is %d, want %d", pts, want)
	}
	// The timestamp is 33 bits wide and wraps, as MPEG-2 timestamps do.
	wrapped := timestampTag(int64(1<<33)*44100/90000+44100, 44100)
	if pts := binary.BigEndian.Uint64(wrapped[len(wrapped)-8:]); pts >= 1<<33 {
		t.Errorf("timestamp %d does not fit 33 bits", pts)
	}
}

func TestEncoderTurnsPCMIntoFrames(t *testing.T) {
	for name, codec := range map[string]Codec{"aac": AAC, "mp3": MP3} {
		t.Run(name, func(t *testing.T) { framesArriveLive(t, codec) })
	}
}

func framesArriveLive(t *testing.T, codec Codec) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is not installed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	encoder, err := StartEncoder(ctx, "", codec, 44100, 64)
	if err != nil {
		t.Fatal(err)
	}
	// Frames must arrive while the input is still open, or a live stream would starve.
	second := make([]byte, 44100*2)
	for range 3 {
		if _, err := encoder.Write(second); err != nil {
			t.Fatal(err)
		}
	}
	frames := 0
	deadline := time.After(10 * time.Second)
	for frames < 86 {
		select {
		case frame, open := <-encoder.Frames:
			if !open {
				t.Fatalf("the encoder stopped after %d frames", frames)
			}
			if frame[0] != 0xFF || frame[1]&0xE0 != 0xE0 {
				t.Fatalf("frame %d has no sync word", frames)
			}
			// Silence at a constant bit rate is as large as speech.
			if codec == MP3 && len(frame) < 200 {
				t.Fatalf("an MP3 frame of silence is only %d bytes", len(frame))
			}
			frames++
		case <-deadline:
			t.Fatalf("only %d frames arrived while the input was open", frames)
		}
	}
	if err := encoder.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestListenersHearEveryFrameUntilTheyFallBehind(t *testing.T) {
	broadcast := NewBroadcast(3)
	for i := range 5 {
		broadcast.Add([]byte{byte(100 + i)})
	}
	recent, keen, stopKeen := broadcast.Listen()
	if !bytes.Equal(recent, []byte{102, 103, 104}) {
		t.Errorf("a new listener is led in with % d, want the last three frames", recent)
	}
	_, slow, stopSlow := broadcast.Listen()
	defer stopSlow()

	for i := range listenerBuffer + 10 {
		broadcast.Add([]byte{byte(i)})
		if frame := <-keen; frame[0] != byte(i) {
			t.Fatalf("frame %d arrived as %d", i, frame[0])
		}
	}
	heard := 0
	for range slow {
		heard++
	}
	if heard != listenerBuffer {
		t.Errorf("a listener that never read heard %d frames before it was dropped, want %d", heard, listenerBuffer)
	}

	stopKeen()
	stopKeen()
	if _, open := <-keen; open {
		t.Error("a stopped listener is still open")
	}
	_, late, _ := broadcast.Listen()
	broadcast.Close()
	if _, open := <-late; open {
		t.Error("closing the broadcast left a listener waiting")
	}
	if _, after, _ := broadcast.Listen(); func() bool { _, open := <-after; return open }() {
		t.Error("a listener who arrives after the end waits for ever")
	}
}
