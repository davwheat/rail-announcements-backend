package helppoint

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata"

	"rail-announcements-backend/internal/ketech"
	"rail-announcements-backend/internal/plan"
)

// FilePrefix is the directory of the voice that every help point clip is in.
var FilePrefix = ketech.Phil.FilePrefix

const (
	beforeServiceDelay   = 2000
	beforeSentenceDelay  = 1000
	beforeFollowUpDelay  = 500
	beforeFormationDelay = 250
)

var london = mustLoadLocation("Europe/London")

func mustLoadLocation(name string) *time.Location {
	location, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return location
}

var operators = map[string]string{
	"AW": "transport for wales", "CC": "c2c", "CH": "chiltern railways", "CS": "caledonian sleeper",
	"EM": "east midlands railway", "ES": "eurostar", "GC": "grand central", "GN": "great northern",
	"GR": "london north eastern railway", "GW": "great western railway", "GX": "gatwick express",
	"HT": "hull trains", "HX": "heathrow express", "IL": "island line", "LD": "lumo", "LE": "greater anglia",
	"LF": "lumo", "LO": "london overground", "ME": "merseyrail", "NT": "northern", "SE": "southeastern",
	"SN": "southern", "SR": "scotrail", "SW": "south western railway", "TL": "thameslink",
	"TP": "transpennine express", "TW": "tyne and wear metro", "VT": "avanti west coast", "XC": "crosscountry",
	"XR": "elizabeth line",
}

// lnwrDestinations tell London Northwestern Railway's trains from West
// Midlands Railway's, which share the operator code LM.
var lnwrDestinations = []string{"EUS", "CRE", "BDM", "SAA", "MKC", "TRI", "LIV", "NMP", "MIA", "MAN"}

// announcement collects clips. A clip with no recording is left out when the
// plan is rendered, so only a choice between recordings has to ask exists.
type announcement struct {
	clips  []plan.Clip
	exists func(id string) bool
}

func (a *announcement) say(ids ...string) {
	a.clips = append(a.clips, plan.IDs(ids...)...)
}

func (a *announcement) sayAfter(delay int, id string) {
	a.clips = append(a.clips, plan.Clip{ID: id, Delay: delay})
}

func (a *announcement) plan() plan.Plan {
	return plan.Plan{Clips: a.clips, MissingAudioMode: plan.PlaySilence}
}

// Unavailable is what a help point says when it can't read the board.
func Unavailable() plan.Plan {
	a := announcement{}
	a.say("w.we regret that the information facility is not in operation")
	a.sayAfter(beforeSentenceDelay, "w.our apologies for any inconvenience caused")
	a.sayAfter(beforeSentenceDelay, "m.please check timetable display information")
	a.say("e.and listen for further announcements")
	return a.plan()
}

// Departures speaks a station's board. exists reports whether Phil has a
// recording of a clip.
func Departures(crs string, services []Service, exists func(id string) bool) plan.Plan {
	a := announcement{exists: exists}
	a.say("s.this is", "station.e."+crs)
	if len(services) == 0 {
		a.sayAfter(beforeSentenceDelay, "w.there are no more services from this station today")
	}

	perPlatform := map[string]int{}
	for _, service := range services {
		switch {
		case service.Cancelled:
			a.cancelled(service)
		case service.PlannedDep != nil:
			perPlatform[service.Platform]++
			a.departing(service, perPlatform[service.Platform])
		case service.PlannedArr != nil:
			a.terminating(service)
		}
	}

	a.sayAfter(beforeServiceDelay, "w.i hope youll have an enjoyable journey")
	a.sayAfter(beforeSentenceDelay, "w.thank you for choosing to travel by rail")
	return a.plan()
}

// departing announces a service as the nth from its platform. Services with
// no platform yet count together, and are announced without one.
func (a *announcement) departing(service Service, nth int) {
	combined := "s.the next service from platform " + service.Platform + " will be the"
	switch {
	case nth > 1:
		a.sayAfter(beforeServiceDelay, "s.the")
		a.say("m.ordinal "+strconv.Itoa(nth), "m.service-2")
		if service.Platform != "" {
			a.say("m.from", "m.platform", number(service.Platform))
		}
		a.say("m.will be the")
	case service.Platform == "":
		a.sayAfter(beforeServiceDelay, "s.the next train is the")
	case a.exists(combined):
		a.sayAfter(beforeServiceDelay, combined)
	default:
		a.sayAfter(beforeServiceDelay, "s.the next service from platform")
		a.say(number(service.Platform), "m.will be the")
	}

	a.time(*service.PlannedDep)
	a.operator(service, "service to")
	a.destinations(service, "e")
	if service.CoachCount != nil && *service.CoachCount > 0 {
		a.sayAfter(beforeFormationDelay, "s.this train is formed of")
		a.say("platform.s."+strconv.Itoa(*service.CoachCount), "e.coaches")
	}
	a.delay(service)
}

func (a *announcement) terminating(service Service) {
	combined := "s.the next service to arrive at platform " + service.Platform
	switch {
	case service.Platform == "":
		a.sayAfter(beforeServiceDelay, "s.the next train")
	case a.exists(combined):
		a.sayAfter(beforeServiceDelay, combined)
	default:
		a.sayAfter(beforeServiceDelay, "s.the next train")
		a.say("m.from", "m.platform", number(service.Platform))
	}
	a.say("m.will be the")
	a.time(*service.PlannedArr)
	a.operator(service, "service from")
	a.origins(service)
	a.sayAfter(beforeFollowUpDelay, "w.this train terminates here")
}

// cancelled names a service by the time and route it would have run: its
// departure, or its arrival when it would have terminated here.
func (a *announcement) cancelled(service Service) {
	a.sayAfter(beforeServiceDelay, "s.im sorry to announce that the")
	if service.PlannedDep != nil {
		a.time(*service.PlannedDep)
		a.operator(service, "service to")
		// "Has been cancelled" follows, so the last station doesn't end the sentence.
		a.destinations(service, "m")
	} else {
		if service.PlannedArr != nil {
			a.time(*service.PlannedArr)
		}
		a.operator(service, "service from")
		a.origins(service)
	}
	a.say("e.has been cancelled")
	if reason := a.reason(service.CancelReasonCode); len(reason) > 0 {
		a.say("m.due to")
		a.say(reason...)
	}
}

func (a *announcement) delay(service Service) {
	expected := service.ExpDep
	if expected == nil {
		return
	}
	late := lateness(*service.PlannedDep, expected)
	reason := a.reason(service.LateReasonCode)
	switch {
	case late >= time.Minute:
		a.sayAfter(beforeFollowUpDelay, "s.this service-2")
		a.say("m.is delayed by approximately")
		a.duration(late)
		if len(reason) > 0 {
			a.say("m.due to")
			a.say(reason...)
		}
	case expected.Delayed:
		a.sayAfter(beforeFollowUpDelay, "s.this service-2")
		if len(reason) > 0 {
			a.say("m.is being delayed due to")
			a.say(reason...)
		} else {
			a.say("e.is being delayed")
		}
		a.sayAfter(beforeFollowUpDelay, "e.please listen for further announcements")
	}
}

// lateness is how far a forecast is behind the planned time, or zero when the
// forecast can't be read.
func lateness(planned time.Time, expected *BoardTime) time.Duration {
	planned = planned.In(london)
	forecast, err := time.ParseInLocation("15:04:05", expected.T, london)
	if err != nil {
		if forecast, err = time.ParseInLocation("15:04", expected.T, london); err != nil {
			return 0
		}
	}
	resolved := time.Date(planned.Year(), planned.Month(), planned.Day(), forecast.Hour(), forecast.Minute(), forecast.Second(), 0, london)
	// A forecast has no date. One far before the planned time is for the next
	// day: a train planned at 23:58 and expected at 00:10.
	if resolved.Before(planned.Add(-12 * time.Hour)) {
		resolved = resolved.AddDate(0, 0, 1)
	}
	return resolved.Sub(planned)
}

// duration says a length of time to the nearest minute: "one hour and five
// minutes".
func (a *announcement) duration(length time.Duration) {
	total := max(1, int((length+30*time.Second)/time.Minute))
	hours, minutes := total/60, total%60
	if hours > 0 {
		word := "hours"
		if hours == 1 {
			word = "hour"
		}
		a.say(number(strconv.Itoa(hours)))
		if minutes > 0 {
			a.say("m."+word, "m.and")
		} else {
			a.say("e." + word)
		}
	}
	if minutes > 0 {
		word := "minutes"
		if minutes == 1 {
			word = "minute"
		}
		a.say(number(strconv.Itoa(minutes)), "e."+word)
	}
}

// reason returns the clips for a Darwin reason code. It returns none unless
// every clip is recorded, so that "due to" is never left without a reason.
func (a *announcement) reason(code string) []string {
	clips := []string(ketech.Phil.DelayCodes[strings.TrimSpace(code)])
	if slices.ContainsFunc(clips, func(id string) bool { return !a.exists(id) }) {
		return nil
	}
	return clips
}

// number says a platform number, or a count, in the middle of a sentence. The
// platform recordings stop at 20, and the minutes of the clock carry on.
func number(value string) string {
	value = strings.ToLower(value)
	switch n, _ := strconv.Atoi(value); {
	case value == "0":
		return "m.0"
	case n >= 21:
		return "mins.m." + value
	}
	return "platform.s." + value
}

func (a *announcement) time(t time.Time) {
	t = t.In(london)
	hour, minute := fmt.Sprintf("%02d", t.Hour()), fmt.Sprintf("%02d", t.Minute())
	if hour == "00" {
		hour = "00 - midnight"
	}
	if minute == "00" {
		minute = "00 - hundred"
	}
	a.say("hour.s."+hour, "mins.m."+minute)
}

// operator says the operator and then suffix, which is "service to" or
// "service from". Most operators have one recording of both together.
func (a *announcement) operator(service Service, suffix string) {
	name := operators[service.TOC]
	if service.TOC == "LM" {
		name = "west midlands railway"
		if len(service.Destinations) > 0 {
			if crs := service.Destinations[0].CRS; crs != nil && slices.Contains(lnwrDestinations, *crs) {
				name = "london northwestern railway"
			}
		}
	}
	switch combined := "toc.m." + name + " " + suffix; {
	case name == "":
		a.say("m." + suffix)
	case a.exists(combined):
		a.say(combined)
	default:
		a.say("toc.m."+name, "m."+suffix)
	}
}

func (a *announcement) origins(service Service) {
	for i, origin := range service.Origins {
		if origin.CRS == nil || *origin.CRS == "" {
			continue
		}
		last := i == len(service.Origins)-1
		if last && i > 0 {
			a.say("m.and")
		}
		if last {
			a.say("station.e." + *origin.CRS)
		} else {
			a.say("station.m." + *origin.CRS)
		}
	}
}

// destinations says where a service goes, and by way of where. finalInflection
// is the inflection of the last station: "e" when it ends the sentence.
func (a *announcement) destinations(service Service, finalInflection string) {
	for i, destination := range service.Destinations {
		if destination.CRS == nil || *destination.CRS == "" {
			continue
		}
		last := i == len(service.Destinations)-1
		if last && i > 0 {
			a.say("m.and")
		}
		if destination.Via == nil {
			if last {
				a.say("station." + finalInflection + "." + *destination.CRS)
			} else {
				a.say("station.m." + *destination.CRS)
			}
			continue
		}
		a.say("station.m."+*destination.CRS, "m.via")
		for j, via := range destination.Via.Locs {
			lastVia := j == len(destination.Via.Locs)-1
			if lastVia && j > 0 {
				a.say("m.and")
			}
			if last && lastVia {
				a.say("station." + finalInflection + "." + via)
			} else {
				a.say("station.m." + via)
			}
		}
	}
}
