// Package system is the seam between the HTTP API and the announcement systems.
//
// The website is moving its systems here one at a time. For each, the website
// posts the state of one of its tabs, and the system answers with the clips
// that tab would play. Only the KeTech station voices are ported so far, and a
// new system joins by implementing System and registering in All.
package system

import (
	"encoding/json"

	"rail-announcements-backend/internal/ketech"
	"rail-announcements-backend/internal/plan"
)

type System interface {
	// ID is the website's ID for the system, such as AMEY_PHIL_V1.
	ID() string
	Name() string
	// FilePrefix is the system's directory in the audio library.
	FilePrefix() string
	// Announcements are the website's tab IDs that Plan accepts.
	Announcements() []string
	// Plan builds what the tab would play in the given state, which is the
	// tab's own option object as the website holds it.
	Plan(announcement string, state json.RawMessage) (plan.Plan, error)
}

type ketechSystem struct{ voice *ketech.Voice }

func (s ketechSystem) ID() string              { return s.voice.ID }
func (s ketechSystem) Name() string            { return s.voice.Name }
func (s ketechSystem) FilePrefix() string      { return s.voice.FilePrefix }
func (s ketechSystem) Announcements() []string { return ketech.Announcements }
func (s ketechSystem) Plan(announcement string, state json.RawMessage) (plan.Plan, error) {
	return s.voice.PlanState(announcement, state)
}

// All lists every system this service can speak for.
var All = []System{ketechSystem{ketech.Phil}, ketechSystem{ketech.Celia}}

func ByID(id string) System {
	for _, s := range All {
		if s.ID() == id {
			return s
		}
	}
	return nil
}
