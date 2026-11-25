package hnet

type PlayerCollection struct {
	idMap   map[uint32]*Player
	nameMap map[string]*Player
}

func (collection *PlayerCollection) Add(player *Player) {
	collection.idMap[player.Info.Id] = player
	collection.nameMap[player.Info.Name] = player
}

// Remove detaches the player from the collection. It is a no-op when the
// player is not present, so repeated disconnect/cleanup paths stay safe.
func (collection *PlayerCollection) Remove(player *Player) {
	// Only delete the name mapping if it still points at this exact player.
	// A re-login of the same account could have registered a new connection
	// in the meantime, and we must not remove that fresh entry.
	if current, ok := collection.idMap[player.Info.Id]; ok && current == player {
		delete(collection.idMap, player.Info.Id)
	}

	if current, ok := collection.nameMap[player.Info.Name]; ok && current == player {
		delete(collection.nameMap, player.Info.Name)
	}
}

func (collection *PlayerCollection) Count() int {
	return len(collection.idMap)
}

func (collection *PlayerCollection) ByID(id uint32) *Player {
	if val, ok := collection.idMap[id]; ok {
		return val
	}

	return nil
}

func (collection *PlayerCollection) ByName(name string) *Player {
	if val, ok := collection.nameMap[name]; ok {
		return val
	}

	return nil
}

// Ptrs returns a snapshot of the currently contained players as pointers.
// Callers must hold the server lock while using the collection.
func (collection *PlayerCollection) Ptrs() []*Player {
	players := make([]*Player, 0, len(collection.idMap))

	for _, player := range collection.idMap {
		players = append(players, player)
	}

	return players
}

// Broadcast sends a packet to every contained player. It must be called with
// the server lock held: the snapshot and the writes are then atomic relative
// to disconnects, so nothing is ever sent to a closed connection.
func (collection *PlayerCollection) Broadcast(packetId uint32, packet Serializable) {
	for _, player := range collection.Ptrs() {
		player.LogOutgoingPacket(packetId, packet)
		player.SendPacket(packetId, packet)
	}
}

func NewPlayerCollection() PlayerCollection {
	return PlayerCollection{
		idMap:   make(map[uint32]*Player),
		nameMap: make(map[string]*Player),
	}
}
