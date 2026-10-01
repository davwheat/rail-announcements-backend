package ketech

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	_ "time/tzdata"

	"rail-announcements-backend/internal/feed"
	"rail-announcements-backend/internal/plan"
)

// Preferences are the listener's choices that change what is said.
type Preferences struct {
	// Chime is empty for the voice's own default.
	Chime                            Chime                 `json:"chime"`
	UseLegacyTocNames                bool                  `json:"useLegacyTocNames"`
	AnnounceViaPoints                bool                  `json:"announceViaPoints"`
	AnnounceShortPlatformsAfterSplit bool                  `json:"announceShortPlatformsAfterSplit"`
	FastTrainApproaching             bool                  `json:"fastTrainApproaching"`
	DaktronicsFanfare                bool                  `json:"daktronicsFanfare"`
	MissingAudioMode                 plan.MissingAudioMode `json:"missingAudioMode"`
}

// Spoken is a live announcement as clips. Fallback, when set, says the same
// with less, for when a clip of Plan turns out not to exist.
type Spoken struct {
	Plan     plan.Plan
	Fallback *plan.Plan
}

var london = mustLoadLocation("Europe/London")

func mustLoadLocation(name string) *time.Location {
	location, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return location
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func stationAudio(location feed.Location) (string, error) {
	if code := StationOverride(location.TPL); code != "" {
		return code, nil
	}
	if code := deref(location.CRS); code != "" {
		return code, nil
	}
	name := deref(location.Name)
	if name == "" {
		name = location.TPL
	}
	return "", fmt.Errorf("No station audio identity for %s", name)
}

// hasActivity looks for one of Darwin's fixed two-character activity codes, so
// that "R" does not match within "RM" or "RR".
func hasActivity(activities *string, code string) bool {
	if activities == nil {
		return false
	}
	for i := 0; i < len(*activities); i += 2 {
		if strings.TrimSpace((*activities)[i:min(i+2, len(*activities))]) == code {
			return true
		}
	}
	return false
}

// ownDestination is where this train itself goes. An endpoint with an
// associated RID belongs to a portion that joins or divides.
func ownDestination(m feed.Movement) feed.Endpoint {
	return ownEndpoint(m.Destinations)
}

func ownEndpoint(endpoints []feed.Endpoint) feed.Endpoint {
	for _, endpoint := range endpoints {
		if deref(endpoint.AssocRID) == "" {
			return endpoint
		}
	}
	return endpoints[0]
}

// runningPortions keeps the endpoints of portions that run today. The feed
// describes an unknown associate by no station at all.
func runningPortions(m feed.Movement, endpoints []feed.Endpoint) []feed.Endpoint {
	var out []feed.Endpoint
	for _, endpoint := range endpoints {
		rid := deref(endpoint.AssocRID)
		if rid != "" && slices.ContainsFunc(m.Portions, func(p feed.Portion) bool { return p.RID == rid && p.Available && !p.Cancelled }) {
			out = append(out, endpoint)
		}
	}
	return out
}

// announcedDestination is the station the train is announced to. The feed's
// via points lie on the route to the real destination, so a false destination
// has none.
func announcedDestination(m feed.Movement) feed.Endpoint {
	own := ownDestination(m)
	if m.FalseDestination != nil {
		own.Location = *m.FalseDestination
		own.Via = nil
	}
	return own
}

func announcedDestinations(m feed.Movement) []feed.Endpoint {
	return append([]feed.Endpoint{announcedDestination(m)}, runningPortions(m, m.Destinations)...)
}

func announcedOrigins(m feed.Movement) []feed.Endpoint {
	return append([]feed.Endpoint{ownEndpoint(m.Origins)}, runningPortions(m, m.Origins)...)
}

func isPassengerCall(call feed.Call) bool {
	return deref(call.CRS) != "" && !call.Operational && !call.Cancelled && !hasActivity(call.Activities, "U")
}

func passengerCalls(calls []feed.Call) []feed.Call {
	var out []feed.Call
	for _, call := range calls {
		if isPassengerCall(call) {
			out = append(out, call)
		}
	}
	return out
}

// onwardCalls are a portion's calls from the division point. Without that
// point, nothing says which calls are still ahead of the train.
func onwardCalls(p feed.Portion) []feed.Call {
	start := slices.IndexFunc(p.Calls, func(call feed.Call) bool {
		return call.TPL == p.At.TPL || (deref(call.CRS) != "" && deref(call.CRS) == deref(p.At.CRS))
	})
	if start == -1 {
		return nil
	}
	return p.Calls[start:]
}

func crsOf[T any](items []T, location func(T) feed.Location) []string {
	out := make([]string, len(items))
	for i, item := range items {
		out[i] = deref(location(item).CRS)
	}
	return out
}

func swapEnds(shortPlatform string) string {
	if strings.HasPrefix(shortPlatform, "front") {
		return strings.Replace(shortPlatform, "front", "rear", 1)
	}
	return strings.Replace(shortPlatform, "rear", "front", 1)
}

func plainPoints(calls []feed.Call, keep func(feed.Call) bool, withRequestStops bool) ([]CallingPoint, error) {
	var out []CallingPoint
	for _, stop := range calls {
		if !keep(stop) {
			continue
		}
		code, err := stationAudio(stop.Location)
		if err != nil {
			return nil, err
		}
		point := CallingPoint{CRSCode: code, Name: deref(stop.Name), RandomID: stop.ID}
		if withRequestStops {
			point.RequestStop = hasActivity(stop.Activities, "R")
		}
		out = append(out, point)
	}
	return out, nil
}

// CallingPoints lists the stops still ahead of the train, in the order it
// makes them, with what each stop needs saying about it.
func CallingPoints(m feed.Movement) ([]CallingPoint, error) {
	train := ShortPlatformTrain{
		OperatorCode:        deref(m.OperatorCode),
		Length:              m.CoachCount,
		Origins:             crsOf(m.Origins, func(e feed.Endpoint) feed.Location { return e.Location }),
		Destinations:        crsOf(m.Destinations, func(e feed.Endpoint) feed.Location { return e.Location }),
		SubsequentLocations: crsOf(m.CallingPoints, func(c feed.Call) feed.Location { return c.Location }),
	}
	// Coach preferences reverse at each reversal, including one at an operational call.
	reversed := m.ReverseFormation != nil && *m.ReverseFormation
	destination := announcedDestination(m)
	// The endpoint and the call can name one station by different TIPLOCs.
	terminus := func(call feed.Call) bool {
		return call.TPL == destination.TPL || (deref(destination.CRS) != "" && deref(call.CRS) == deref(destination.CRS))
	}
	// A train on a circular route calls at a false destination again on its way
	// to the real one, so the calling points end at the first call there. The
	// real destination is the last call at it.
	destinationIndex := -1
	for i, call := range m.CallingPoints {
		if terminus(call) {
			destinationIndex = i
			if m.FalseDestination != nil {
				break
			}
		}
	}

	result := []CallingPoint{}
	for i, call := range m.CallingPoints {
		if hasActivity(call.Activities, "RM") {
			reversed = !reversed
		}
		terminates := i == destinationIndex
		if !isPassengerCall(call) {
			if terminates {
				break
			}
			continue
		}
		var portions []feed.Portion
		for _, portion := range m.Portions {
			if portion.At.TPL == call.TPL && portion.Available && !portion.Cancelled {
				portions = append(portions, portion)
			}
		}
		if terminates && len(portions) == 0 {
			break
		}

		callTrain := train
		if call.CoachCount != nil {
			callTrain.Length = call.CoachCount
		}
		short := ShortPlatform(deref(call.CRS), call.Platform.Number, callTrain)
		if reversed && short != "" {
			short = swapEnds(short)
		}
		code, err := stationAudio(call.Location)
		if err != nil {
			return nil, err
		}
		point := CallingPoint{
			CRSCode:       code,
			Name:          deref(call.Name),
			RandomID:      call.ID,
			RequestStop:   hasActivity(call.Activities, "R"),
			ShortPlatform: short,
		}

		if divide := slices.IndexFunc(portions, func(p feed.Portion) bool { return p.Category == "VV" }); divide != -1 {
			portion := portions[divide]
			point.SplitType = "splits"
			position := deref(portion.Position)
			if position == "" {
				switch {
				case call.DetachFront == nil:
					position = "unknown"
				case *call.DetachFront:
					position = "front"
				default:
					position = "rear"
				}
			}
			form := "unknown"
			if portion.CoachCount != nil && *portion.CoachCount != 0 && slices.Contains([]string{"front", "rear", "middle"}, position) {
				form = fmt.Sprintf("%s.%d", position, *portion.CoachCount)
			}
			point.SplitForm = &form
			point.SplitCallingPoints, err = plainPoints(passengerCalls(onwardCalls(portion)), func(stop feed.Call) bool { return stop.TPL != call.TPL }, true)
			if err != nil {
				return nil, err
			}
		}

		bus := slices.IndexFunc(portions, func(p feed.Portion) bool {
			return (p.Category == "NP" || p.Category == "LK") && deref(p.Mode) == "bus"
		})
		if bus != -1 {
			point.ContinuesAsRrbAfterHere = true
		}
		// The terminus is spoken as the destination, so it never joins the calling points as well.
		if !terminates {
			result = append(result, point)
		}
		if bus != -1 {
			onward, err := plainPoints(passengerCalls(onwardCalls(portions[bus])), func(stop feed.Call) bool { return stop.TPL != call.TPL && !terminus(stop) }, false)
			if err != nil {
				return nil, err
			}
			result = append(result, onward...)
		}
		if terminates {
			break
		}
	}
	return result, nil
}

func viaPoints(endpoints []feed.Endpoint, m feed.Movement, announce bool) ([][]CallingPoint, error) {
	out := make([][]CallingPoint, len(endpoints))
	for i, endpoint := range endpoints {
		out[i] = []CallingPoint{}
		if !announce || endpoint.Via == nil {
			continue
		}
		for _, crs := range endpoint.Via.Locs {
			point := CallingPoint{CRSCode: crs, RandomID: crs}
			if call := slices.IndexFunc(m.CallingPoints, func(c feed.Call) bool { return c.CRS != nil && *c.CRS == crs }); call != -1 {
				code, err := stationAudio(m.CallingPoints[call].Location)
				if err != nil {
					return nil, err
				}
				point.CRSCode, point.Name = code, deref(m.CallingPoints[call].Name)
			}
			out[i] = append(out[i], point)
		}
	}
	return out, nil
}

func delayMillis(t feed.Times) (int64, bool) {
	if !t.Estimated.Valid || !t.Planned.Valid {
		return 0, false
	}
	// The website reads both instants at millisecond precision.
	return t.Estimated.Time.UnixMilli() - t.Planned.Time.UnixMilli(), true
}

// LiveTrainOptions turns a movement into the state the next train tab would
// hold for it.
func (v *Voice) LiveTrainOptions(m feed.Movement, prefs Preferences, platform string) (TrainOptions, error) {
	if !m.Departure.Planned.Valid || len(m.Destinations) == 0 {
		return TrainOptions{}, errors.New("Departure time or destination unavailable")
	}
	departs := m.Departure.Planned.Time.In(london)
	hour, minute := departs.Format("15"), departs.Format("04")
	if hour == "00" {
		hour = "00 - midnight"
	}
	if minute == "00" {
		minute = "00 - hundred-hours"
	}
	delay, _ := delayMillis(m.Departure)

	loading := -1.0
	if m.LoadingPercent != nil {
		loading = float64(*m.LoadingPercent)
	} else {
		sum, known := 0, 0
		for _, coach := range m.Coaches {
			if coach.LoadingPercent != nil {
				sum += *coach.LoadingPercent
				known++
			}
		}
		if known > 0 {
			loading = float64(sum) / float64(known)
		}
	}

	destination := announcedDestination(m)
	terminating, err := stationAudio(destination.Location)
	if err != nil {
		return TrainOptions{}, err
	}
	vias, err := viaPoints([]feed.Endpoint{destination}, m, prefs.AnnounceViaPoints)
	if err != nil {
		return TrainOptions{}, err
	}
	callingAt, err := CallingPoints(m)
	if err != nil {
		return TrainOptions{}, err
	}
	notCalling, err := plainPoints(m.CallingPoints, func(call feed.Call) bool {
		return deref(call.CRS) != "" && !call.Operational && call.Cancelled && !hasActivity(call.Activities, "U")
	}, false)
	if err != nil {
		return TrainOptions{}, err
	}

	chime := prefs.Chime
	if chime == "" {
		chime = v.DefaultChime
	}
	originCRS := ""
	if len(m.Origins) > 0 {
		originCRS = deref(m.Origins[0].CRS)
	}
	o := TrainOptions{
		FromLive:         true,
		MissingAudioMode: prefs.MissingAudioMode,
		Chime:            chime,
		Hour:             hour,
		Min:              minute,
		IsDelayed:        m.Departure.UnknownDelay || (m.Departure.Estimated.Valid && delay >= 5*60_000),
		Toc: v.TocForLiveTrain(deref(m.OperatorName), deref(m.OperatorCode), originCRS, deref(ownDestination(m).CRS),
			prefs.UseLegacyTocNames, deref(m.UID)),
		Platform:                         platform,
		TerminatingStationCode:           terminating,
		Vias:                             vias[0],
		CallingAt:                        callingAt,
		FirstClassLocation:               "none",
		Coaches:                          "None",
		ServiceLoading:                   "none",
		AnnounceShortPlatformsAfterSplit: prefs.AnnounceShortPlatformsAfterSplit,
		NotCallingAtStations:             notCalling,
	}
	if m.CoachCount != nil && *m.CoachCount != 0 {
		o.Coaches = fmt.Sprintf("%d coaches", *m.CoachCount)
	}
	if loading > 70 {
		o.ServiceLoading = "full and standing"
	}
	return o, nil
}

func (v *Voice) liveRoute(m feed.Movement, prefs Preferences, o TrainOptions) (RouteOptions, error) {
	destinations := announcedDestinations(m)
	vias, err := viaPoints(destinations, m, prefs.AnnounceViaPoints)
	if err != nil {
		return RouteOptions{}, err
	}
	route := RouteOptions{
		Chime: o.Chime, Hour: o.Hour, Min: o.Min, IsDelayed: o.IsDelayed, Toc: o.Toc, MissingAudioMode: o.MissingAudioMode,
		Destinations: Destinations{Live: true},
	}
	for i, destination := range destinations {
		code, err := stationAudio(destination.Location)
		if err != nil {
			return RouteOptions{}, err
		}
		route.Destinations.Stations = append(route.Destinations.Stations, code)
		route.Destinations.Vias = append(route.Destinations.Vias, crsCodes(vias[i]))
	}
	return route, nil
}

// Announce turns a live announcement into clips for one platform, which must
// be a platform this voice can say: see [Voice.AudioPlatform].
func (v *Voice) Announce(a feed.Announcement, prefs Preferences, platform string) (Spoken, error) {
	if a.Type == feed.Passing {
		p, err := v.FastTrain(FastTrainOptions{
			Chime:                prefs.Chime,
			DaktronicsFanfare:    prefs.DaktronicsFanfare,
			Platform:             platform,
			FastTrainApproaching: prefs.FastTrainApproaching,
			MissingAudioMode:     prefs.MissingAudioMode,
		})
		return Spoken{Plan: p}, err
	}

	m := a.Details
	o, err := v.LiveTrainOptions(m, prefs, platform)
	if err != nil {
		return Spoken{}, err
	}
	single := func(p plan.Plan, err error) (Spoken, error) { return Spoken{Plan: p}, err }

	switch a.Type {
	case feed.Next:
		return single(v.NextTrain(o))

	case feed.Standing:
		o.ThisStationCode = deref(m.Station.CRS)
		o.MindTheGap = IsMindTheGap(deref(m.Station.CRS), m.Platform.Number)
		return single(v.StandingTrain(o))

	case feed.Approaching:
		if len(m.Origins) == 0 {
			return Spoken{}, errors.New("Origin unavailable")
		}
		var origins []string
		for _, origin := range announcedOrigins(m) {
			code, err := stationAudio(origin.Location)
			if err != nil {
				return Spoken{}, err
			}
			origins = append(origins, code)
		}
		route, err := v.liveRoute(m, prefs, o)
		if err != nil {
			return Spoken{}, err
		}
		return single(v.TrainApproaching(ApproachingOptions{RouteOptions: route, Platform: platform, Origins: origins}))

	case feed.Disrupted:
		reason := m.DelayReason
		if m.Cancelled {
			reason = m.CancelReason
		}
		millis, known := delayMillis(m.Departure)
		minutes := int(floorDiv(millis, 60_000))
		// Without a delay to count, the generic wording replaces "delayed by approximately".
		disruption := "delayedBy"
		if m.Departure.UnknownDelay || !known || minutes <= 0 {
			disruption = "delay"
		}
		if m.Cancelled {
			disruption = "cancel"
		}
		route, err := v.liveRoute(m, prefs, o)
		if err != nil {
			return Spoken{}, err
		}
		options := DisruptedOptions{RouteOptions: route, DisruptionType: disruption, DelayTime: fmt.Sprint(max(0, minutes))}
		// The mapping holds finished clip IDs, which the voice plays as they are only in list form.
		if clips := v.DelayCodes[deref(reason.Code)]; len(clips) > 0 {
			options.Reason = DisruptionReason{Clips: clips, List: true}
		}
		spoken, err := single(v.DisruptedTrain(options))
		if err != nil || !options.Reason.given() {
			return spoken, err
		}
		// A reason the voice cannot say must not cost the listener the disruption itself.
		options.Reason = DisruptionReason{}
		fallback, err := v.DisruptedTrain(options)
		spoken.Fallback = &fallback
		return spoken, err

	case feed.PlatformAlteration:
		if deref(a.PreviousPlatform) == "" || deref(a.NewPlatform) == "" {
			return Spoken{}, errors.New("Platform alteration details unavailable")
		}
		oldPlatform, oldOK := v.AudioPlatform(*a.PreviousPlatform)
		newPlatform, newOK := v.AudioPlatform(*a.NewPlatform)
		if !oldOK || !newOK {
			return Spoken{}, errors.New("Platform audio unavailable")
		}
		route, err := v.liveRoute(m, prefs, o)
		if err != nil {
			return Spoken{}, err
		}
		return single(v.PlatformAlteration(AlterationOptions{
			RouteOptions:        route,
			AnnounceOldPlatform: true,
			OldPlatform:         oldPlatform,
			NewPlatform:         newPlatform,
		}))
	}
	return Spoken{}, fmt.Errorf("unknown announcement type %q", a.Type)
}

func floorDiv(a, b int64) int64 {
	q := a / b
	if a%b != 0 && (a < 0) != (b < 0) {
		q--
	}
	return q
}

var mindTheGap = mustLoad[map[string][]string]("mind-the-gap")

func IsMindTheGap(crs string, platform *string) bool {
	return platform != nil && slices.Contains(mindTheGap[crs], *platform)
}
