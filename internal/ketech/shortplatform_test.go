package ketech

import (
	"testing"

	"rail-announcements-backend/internal/feed"
)

func TestCallingPointsTakeTheUnitsGeminiAllocatesToEachCall(t *testing.T) {
	str := func(s string) *string { return &s }
	count := func(n int) *int { return &n }
	call := func(id, crs string, units ...string) feed.Call {
		return feed.Call{ID: id, Location: feed.Location{TPL: crs, CRS: str(crs)}, Platform: feed.Platform{Number: str("1")}, Units: units}
	}
	shawford := call("1", "SHW", "444001", "444002")
	shawford.CoachCount = count(10)
	m := feed.Movement{
		OperatorCode:  str("SW"),
		CoachCount:    count(12),
		Units:         []string{"450001", "450002", "450003"},
		Destinations:  []feed.Endpoint{{Location: feed.Location{TPL: "SOU", CRS: str("SOU")}}},
		CallingPoints: []feed.Call{shawford, call("2", "SWG"), call("3", "SOU")},
	}
	points, err := CallingPoints(m)
	if err != nil {
		t.Fatal(err)
	}
	if len(points) != 2 || points[0].ShortPlatform != "front.4" || points[1].ShortPlatform != "front.4" {
		t.Fatalf("got %+v, want the 444s at Shawford and the 450s at Swaythling each told to join the front 4 coaches", points)
	}
	m.CallingPoints[1].Units = []string{"444001", "450002"}
	m.CallingPoints[1].CoachCount = count(9)
	if points, _ := CallingPoints(m); points[1].ShortPlatform != "front.3" {
		t.Fatalf("got %q at Swaythling for a 444 coupled to a 450, want front.3", points[1].ShortPlatform)
	}
}
