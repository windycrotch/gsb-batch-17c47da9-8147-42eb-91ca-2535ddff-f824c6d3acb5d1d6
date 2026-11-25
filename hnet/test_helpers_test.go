package hnet

import (
	"encoding/binary"

	"github.com/hexis-revival/hexagon/common"
)

// statusChangeStream builds the wire payload for a status change carrying no
// beatmap info (action <= ACTION_AWAY), which serializes to just the user id
// and action fields.
func statusChangeStream(action uint32) *common.IOStream {
	stream := common.NewIOStream([]byte{}, binary.BigEndian)
	stream.WriteU32(0) // user id is populated by the client; handler ignores it
	stream.WriteU32(action)
	return stream
}

// scorePackStream builds the wire payload for an empty replay frame packet.
func scorePackStream() *common.IOStream {
	stream := common.NewIOStream([]byte{}, binary.BigEndian)
	stream.WriteU32(0) // action
	stream.WriteU32(0) // frame count
	return stream
}
