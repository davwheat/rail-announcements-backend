package feed

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

// The .pb files are frames written by the feed service's own encoder, and each .json
// beside one is the same message as the websites' types spell it.
func TestDecodeReadsTheServiceFrames(t *testing.T) {
	for _, name := range []string{"passing", "platform_alteration", "td_unknown", "announcement_audio"} {
		frame, err := os.ReadFile("testdata/" + name + ".pb")
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := Decode(frame)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got, ok := decoded.(*Announcement)
		if !ok {
			t.Fatalf("%s: decoded to %T", name, decoded)
		}

		raw, err := os.ReadFile("testdata/" + name + ".json")
		if err != nil {
			t.Fatal(err)
		}
		var want Announcement
		if err := json.Unmarshal(raw, &want); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(normalise(t, *got), normalise(t, want)) {
			gotJSON, _ := json.MarshalIndent(got, "", " ")
			wantJSON, _ := json.MarshalIndent(want, "", " ")
			t.Errorf("%s differs:\n got %s\nwant %s", name, gotJSON, wantJSON)
		}
	}
}

// normalise compares by value through JSON, because a decoded time and a
// parsed one are equal instants with different representations.
func normalise(t *testing.T, a Announcement) any {
	t.Helper()
	raw, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestDecodeRejectsOtherVersionsAndIgnoresUnknownPayloads(t *testing.T) {
	if _, err := Decode([]byte{0x08, 0x01}); err == nil {
		t.Error("version 1 was accepted")
	}
	if _, err := Decode([]byte("not protobuf")); err == nil {
		t.Error("garbage was accepted")
	}
	decoded, err := Decode([]byte{0x08, 0x02})
	if decoded != nil || err != nil {
		t.Errorf("a message with no payload gave %v, %v", decoded, err)
	}
}
