package internal

type playerManger struct {
	PlayerMap map[string]*Player
}

func NewPlayerManager() *playerManger {
	mgr := new(playerManger)
	mgr.Init()
	return mgr
}

func (self *playerManger) Init() {
	self.PlayerMap = make(map[string]*Player)
}

func (self *playerManger) Get(id string) *Player {
	return self.PlayerMap[id]
}

func (self *playerManger) AddPlayer(player *Player) {
	self.PlayerMap[player.objid] = player
}

func (self *playerManger) DelPlayer(objid string) {
	delete(self.PlayerMap, objid)
}

// Close 在关停保存完成后清空内存中的玩家索引。
// 真正的同步落库由 session.Manager.Shutdown 统一完成，
// 这里不再重复保存，避免同一玩家被写两次。
func (self *playerManger) Close() {
	self.PlayerMap = make(map[string]*Player)
}
