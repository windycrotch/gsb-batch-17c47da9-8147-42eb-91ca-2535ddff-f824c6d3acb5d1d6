package hnet

import (
	"encoding/binary"
	"fmt"
	"net"

	"github.com/hexis-revival/hexagon/common"
)

type Player struct {
	Conn       net.Conn
	Logger     *common.Logger
	Server     *HNetServer
	Client     *ClientInfo
	Info       *UserInfo
	Stats      *UserStats
	Host       *Player
	Spectators PlayerCollection
	// closed marks a connection whose teardown has already run. Guarded by
	// Server.mutex. It makes disconnect/kick idempotent: a replaced
	// connection whose socket then errors out must not be torn down twice.
	closed bool
}

func (player *Player) Send(data []byte) error {
	player.Logger.Verbosef("<- %s", common.FormatBytes(data))
	_, err := player.Conn.Write(data)
	return err
}

func (player *Player) Receive() ([]byte, error) {
	buffer := make([]byte, 1024*1024)
	n, err := player.Conn.Read(buffer)

	if err != nil {
		return nil, err
	}

	buffer = buffer[:n]
	player.Logger.Verbosef("-> %s", common.FormatBytes(buffer))
	return buffer, nil
}

func (player *Player) OnConnect() {
	player.Logger.Debug("-> Connected")
}

// CloseConnection explicitly tears a player down (auth failures, replaced
// logins, protocol violations). The serving goroutine's deferred cleanup
// calls the same idempotent path, so a second invocation is a no-op.
func (player *Player) CloseConnection() {
	player.Server.disconnect(player)
}

func (player *Player) LogIncomingPacket(packetId uint32, packet Serializable) {
	if packet == nil {
		player.Logger.Debugf("-> %d: nil", packetId)
		return
	}

	player.Logger.Debugf("-> %d: %s", packetId, packet.String())
}

func (player *Player) LogOutgoingPacket(packetId uint32, packet Serializable) {
	if packet == nil {
		player.Logger.Debugf("<- %d: nil", packetId)
		return
	}

	player.Logger.Debugf("<- %d: %s", packetId, packet.String())
}

func (player *Player) SendPacketData(packetId uint32, data []byte) error {
	stream := common.NewIOStream([]byte{}, binary.BigEndian)
	stream.WriteU8(0x87)
	stream.WriteU32(packetId)
	stream.WriteU32(uint32(len(data)))
	stream.Write(data)
	return player.Send(stream.Get())
}

func (player *Player) SendPacket(packetId uint32, packet Serializable) error {
	player.LogOutgoingPacket(packetId, packet)
	stream := common.NewIOStream([]byte{}, binary.BigEndian)
	packet.Serialize(stream)
	return player.SendPacketData(packetId, stream.Get())
}

func (player *Player) IsAuthenticated() bool {
	return player.Info.Id != 0 || player.Info.Name != ""
}

func (player *Player) OnLoginSuccess(responsePassword string, userObject *common.User) error {
	server := player.Server

	// Ensure that the stats object exists
	userObject.EnsureStats(player.Server.State)

	// Populate player info & stats
	player.ApplyUserData(userObject)

	server.mutex.Lock()

	// Another connection for this account is online: replace it. The old
	// connection's spectating relations are fully removed during teardown,
	// and this brand-new Player struct starts with no Host and no spectators
	// of its own, so the new connection never inherits the old identity.
	otherUser := server.Players.ByID(uint32(userObject.Id))

	if otherUser != nil {
		_ = otherUser.SendPacketData(SERVER_LOGIN_REVOKED, []byte{})
		server.teardown(otherUser)
	}

	server.Players.Add(player)

	player.Logger.Infof(
		"Login attempt as '%s' with version %s",
		player.Info.Name,
		player.Client.Version.String(),
	)

	player.Logger.SetName(fmt.Sprintf(
		"Player \"%s\"",
		player.Info.Name,
	))

	// Exchange user info with every currently online player while the
	// registry cannot change underneath us.
	for _, other := range server.Players.Ptrs() {
		if other == player {
			continue
		}

		other.SendPacket(SERVER_USER_INFO, player.Info)
		player.SendPacket(SERVER_USER_INFO, other.Info)
	}

	server.mutex.Unlock()

	response := LoginResponse{
		UserId:   player.Info.Id,
		Username: player.Info.Name,
		Client:   player.Client,
		Password: responsePassword,
	}

	// Send login response
	err := player.SendPacket(SERVER_LOGIN_RESPONSE, response)
	if err != nil {
		player.CloseConnection()
		return err
	}

	friendIds, err := player.GetFriendIds()
	if err != nil {
		return err
	}

	// Send friends list
	friendsList := FriendsList{FriendIds: friendIds}
	err = player.SendPacket(SERVER_FRIENDS_LIST, friendsList)
	if err != nil {
		return err
	}

	return nil
}

func (player *Player) OnLoginFailed(reason string) {
	player.Logger.Warningf("Login attempt failed: %s", reason)
	player.CloseConnection()
}

func (player *Player) RevokeLogin() error {
	return player.SendPacketData(SERVER_LOGIN_REVOKED, []byte{})
}

func (player *Player) Refresh() error {
	user, err := common.FetchUserById(
		int(player.Info.Id),
		player.Server.State,
		"Stats",
	)

	if err != nil {
		return err
	}

	return player.ApplyUserData(user)
}

func (player *Player) AddRelationship(targetId uint32, status common.RelationshipStatus) error {
	rel := &common.Relationship{
		UserId:   int(player.Info.Id),
		TargetId: int(targetId),
		Status:   status,
	}

	return common.CreateUserRelationship(rel, player.Server.State)
}

func (player *Player) RemoveRelationship(targetId uint32, status common.RelationshipStatus) error {
	rel, err := common.FetchUserRelationship(
		int(player.Info.Id),
		int(targetId),
		player.Server.State,
	)

	if err != nil {
		return err
	}

	return common.RemoveUserRelationship(rel, player.Server.State)
}

func (player *Player) GetFriendIds() ([]uint32, error) {
	relationships, err := common.FetchUserRelationships(
		int(player.Info.Id),
		common.StatusFriend,
		player.Server.State,
	)

	if err != nil {
		return nil, err
	}

	friends := make([]uint32, 0, len(relationships))

	for _, rel := range relationships {
		friends = append(friends, uint32(rel.TargetId))
	}

	return friends, nil
}

func (player *Player) ApplyUserData(user *common.User) error {
	player.Info.Name = user.Name
	player.Info.Id = uint32(user.Id)
	player.Stats.UserId = uint32(user.Id)
	player.Stats.Rank = uint32(user.Stats.Rank)
	player.Stats.RankedScore = uint64(user.Stats.RankedScore)
	player.Stats.TotalScore = uint64(user.Stats.TotalScore)
	player.Stats.Plays = uint32(user.Stats.Playcount)
	player.Stats.Accuracy = user.Stats.Accuracy
	return nil
}

// StartSpectating makes player watch host. The whole relation change and the
// resulting notifications run under the server lock, so it is atomic with
// respect to the host disconnecting: a spectate request can never leave a
// relation pointing at an offline player. Switching targets first tears down
// the relation with the previous host.
func (player *Player) StartSpectating(host *Player) error {
	player.Server.mutex.Lock()
	defer player.Server.mutex.Unlock()
	return player.startSpectating(host)
}

// startSpectating contains the spectate logic. The caller must hold the
// server lock.
func (player *Player) startSpectating(host *Player) error {
	if player.closed || host.closed {
		return fmt.Errorf("cannot spectate: connection is closed")
	}

	if host == player {
		return fmt.Errorf("cannot spectate own connection")
	}

	// Switching spectating targets (or re-requesting the same one) must not
	// leave the player in the previous host's spectator list.
	oldHost := player.Host

	if oldHost != nil && oldHost != host {
		oldHost.Spectators.Remove(player)

		if !oldHost.closed {
			oldHost.SendPacket(SERVER_STOP_SPECTATING, &SpectateRequest{
				UserId: player.Info.Id,
			})
		}
	}

	player.Host = host
	host.Spectators.Add(player)

	if err := host.SendPacket(SERVER_START_SPECTATING, &SpectateRequest{
		UserId: player.Info.Id,
	}); err != nil {
		player.rollbackSpectating(host, oldHost)
		return err
	}

	if err := player.SendPacket(SERVER_SPECTATE_STATUS_UPDATE, host.Stats.Status); err != nil {
		player.rollbackSpectating(host, oldHost)
		return err
	}

	player.Logger.Infof("Started spectating '%s'", host.Info.Name)
	return nil
}

// rollbackSpectating undoes a failed startSpectating so that a failed
// notification never leaves a half-established relation behind. The caller
// must hold the server lock.
func (player *Player) rollbackSpectating(newHost, oldHost *Player) {
	newHost.Spectators.Remove(player)

	if player.Host == newHost {
		player.Host = oldHost
	}

	if oldHost != nil && oldHost != newHost && !oldHost.closed {
		oldHost.Spectators.Add(player)
	}
}

// StopSpectating detaches the player from the host they are watching. It is
// safe to call when the player is not spectating anybody, when called
// repeatedly, and when the former host has already gone away: in all of
// those cases it simply ends without residual state or broadcasts.
func (player *Player) StopSpectating() error {
	player.Server.mutex.Lock()
	defer player.Server.mutex.Unlock()
	return player.stopSpectating()
}

// stopSpectating contains the detach logic. The caller must hold the server
// lock.
func (player *Player) stopSpectating() error {
	host := player.Host

	if host == nil {
		player.Logger.Debug("Stop spectating requested without an active host")
		return nil
	}

	player.Host = nil

	if !host.closed {
		host.Spectators.Remove(player)

		if err := host.SendPacket(SERVER_STOP_SPECTATING, &SpectateRequest{
			UserId: player.Info.Id,
		}); err != nil {
			return err
		}
	}

	player.Logger.Infof("Stopped spectating")
	return nil
}

func (player *Player) IsSpectating() bool {
	player.Server.mutex.Lock()
	defer player.Server.mutex.Unlock()
	return player.Host != nil
}

func (player *Player) HasSpectators() bool {
	player.Server.mutex.Lock()
	defer player.Server.mutex.Unlock()
	return player.Spectators.Count() > 0
}
