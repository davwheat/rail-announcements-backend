package hls

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestPlaylistCutsSegmentsAndSlides(t *testing.T) {
	clock := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	playlist := NewPlaylist(44100, 2*time.Second, 3, func() time.Time { return clock })
	first := int(clock.Unix())
	frame := []byte{0xFF, 0xF1, 0, 0, 0, 0, 0, 1, 2, 3}

	// 86 frames of 1024 samples are 1.997 s, the nearest whole number of frames to 2 s.
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
	rendered := playlist.Render(func(sequence int) string { return "s/" + time.Unix(int64(sequence), 0).UTC().Format("150405") + ".aac" })
	for _, want := range []string{
		"#EXT-X-VERSION:3\n",
		"#EXT-X-TARGETDURATION:2\n",
		fmt.Sprintf("#EXT-X-MEDIA-SEQUENCE:%d\n", first+7),
		"#EXTINF:1.99692,\ns/100007.aac\n",
		"#EXTINF:1.99692,\ns/100009.aac\n",
		// Segments are dated by the audio before them, whatever the clock said when they were cut.
		"#EXT-X-PROGRAM-DATE-TIME:2026-09-18T10:00:13.978Z\n",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("playlist lacks %q:\n%s", want, rendered)
		}
	}
	if strings.Contains(rendered, "ENDLIST") || strings.Count(rendered, "#EXTINF") != 3 {
		t.Errorf("a live playlist lists its window and never ends:\n%s", rendered)
	}

	if _, ok := playlist.Segment(first); ok {
		t.Error("a segment far behind the window is still held")
	}
	segment, ok := playlist.Segment(first + 9)
	if !ok {
		t.Fatal("the newest segment is missing")
	}
	if !bytes.HasSuffix(segment.Data, frame) || len(segment.Data) <= 86*len(frame) {
		t.Error("the segment is not its tag followed by its frames")
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
