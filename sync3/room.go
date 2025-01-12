package sync3

import (
	"encoding/json"
	"github.com/matrix-org/sliding-sync/internal"

	"github.com/matrix-org/sliding-sync/sync3/caches"
)

type Room struct {
	Name              string            `json:"name,omitempty"`
	RequiredState     []json.RawMessage `json:"required_state,omitempty"`
	Timeline          []json.RawMessage `json:"timeline,omitempty"`
	InviteState       []json.RawMessage `json:"invite_state,omitempty"`
	NotificationCount int64             `json:"notification_count"`
	HighlightCount    int64             `json:"highlight_count"`
	Initial           bool              `json:"initial,omitempty"`
	IsDM              bool              `json:"is_dm,omitempty"`
	JoinedCount       int               `json:"joined_count,omitempty"`
	InvitedCount      int               `json:"invited_count,omitempty"`
	PrevBatch         string            `json:"prev_batch,omitempty"`
	NumLive           int               `json:"num_live,omitempty"`
}

// RoomConnMetadata represents a room as seen by one specific connection (hence one
// specific device).
type RoomConnMetadata struct {
	// We enclose copies of the data kept in the global and user caches. These snapshots
	// represent the state we reported to the connection the last time they requested
	// a sync. Note that we are free to tweak fields within these copies if we want, to
	// report more appropriate data to clients that kept in the caches.
	internal.RoomMetadata
	caches.UserRoomData
	// plus any per-conn data.

	// LastInterestedEventTimestamp is the origin_server_ts of the most recent event
	// seen in the room that this connection is interested in. Connections can specify
	// that they are only in certain event types by providing "bump_event_types" in
	// their sliding sync request.
	//
	// While this conceptually tracks the internal.RoomMetadata.LastMessageTimestamp
	// field, said field can decrease rapidly at any moment---so
	// LastInterestedEventTimestamp can also decrease at any moment. When sorting by
	// recency, this means rooms can suddenly fall down and jump back up the room
	// list. See also the description of this in the React SDK docs:
	//     https://github.com/matrix-org/matrix-react-sdk/blob/526645c79160ab1ad4b4c3845de27d51263a405e/docs/room-list-store.md#tag-sorting-algorithm-recent
	LastInterestedEventTimestamp uint64
}
