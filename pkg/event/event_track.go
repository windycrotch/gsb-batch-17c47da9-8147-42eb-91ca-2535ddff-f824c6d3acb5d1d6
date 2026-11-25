/*
Copyright 2022-2025 The nagare media authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package event

import "github.com/nagare-media/ingest/pkg/media"

type TrackEventType int

const (
	UnknownTrackEvent TrackEventType = iota
	TrackStartEvent
	// TrackEndEvent signals that a track was ingested completely and the upload finished normally. The track is
	// finalized but may still be advertised until the stream ends.
	TrackEndEvent
	// TrackStopEvent signals that a track was stopped prematurely: either its upload failed, or the stream was
	// garbage collected after inactivity. The track must no longer be advertised as active.
	TrackStopEvent
)

type TrackEvent struct {
	Type  TrackEventType
	Track *media.Track
}

func NewTrackEvent(t TrackEventType, trk *media.Track) Event {
	return &TrackEvent{
		Type:  t,
		Track: trk,
	}
}
