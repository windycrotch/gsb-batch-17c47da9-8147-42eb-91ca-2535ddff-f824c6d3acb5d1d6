package internal

import (
	"leaf_server/msg"
	"leaf_server/session"

	"github.com/name5566/leaf/gate"
	"github.com/name5566/leaf/log"
)

const (
	INTATTR_COIN  = 0 //金币
	INTATTR_POWER = 1 //体力

	INTATTR_MAX = 10
)

const (
	STRATTR_NICK = 0 //昵称
	STRATTR_ICON = 1 //头像

	STRATTR_MAX = 2
)

type Player struct {
	objid   string
	agent   gate.Agent
	UID     int64
	Account string
	IntAttr []int    //整型属性
	StrAttr []string //字符串属性

	// active 标记该玩家持有的会话是否仍然有效。
	// 被新登录顶替或连接关闭后置为 false，业务消息入口据此拦截迟到消息。
	active bool

	// testCalled 记录 Test 业务方法被有效执行的次数。
	testCalled int
}

// agentConn 把 gate.Agent 适配为 session.Conn。
// 同一个 gate.Agent 可能在不同入口（登录、消息、关闭）被多次包装，
// 因此相等性按底层 agent 实例判断，而不是按包装体指针判断。
type agentConn struct {
	agent gate.Agent
}

func (c *agentConn) WriteMsg(m interface{}) {
	// 会话层的下线通知转换为协议消息后再经 gate 下发，
	// 保证消息类型已在 processor 注册。
	if kick, ok := m.(*session.KickMsg); ok {
		c.agent.WriteMsg(&msg.KickNotify{Reason: kick.Reason})
		return
	}
	c.agent.WriteMsg(m)
}

func (c *agentConn) Close() { c.agent.Close() }

// Equal 让 session.Manager 能识别“两个包装体背后是同一条连接”。
func (c *agentConn) Equal(other session.Conn) bool {
	o, ok := other.(*agentConn)
	return ok && o.agent == c.agent
}

var _ session.Conn = (*agentConn)(nil)

func CreatePlayer() *Player {
	player := new(Player)
	player.IntAttr = make([]int, INTATTR_MAX)
	player.StrAttr = make([]string, STRATTR_MAX)
	return player
}

func (self *Player) GetIntAttr(index int) int {
	if index < 0 || index >= INTATTR_MAX {
		return 0
	}
	return self.IntAttr[index]
}

func (self *Player) SetIntAttr(index, val int) {
	if index < 0 || index >= INTATTR_MAX {
		return
	}

	self.IntAttr[index] = val
}

func (self *Player) GetStrAttr(index int) string {
	if index < 0 || index >= STRATTR_MAX {
		return ""
	}
	return self.StrAttr[index]
}

func (self *Player) SetStrAttr(index int, val string) {
	if index < 0 || index >= STRATTR_MAX {
		return
	}

	self.StrAttr[index] = val
}

// uidGenerator 生成玩家 UID。生产环境走 UidBuilder（基于 mongodb 自增序列），
// 测试环境可替换为本地计数器，避免依赖真实数据库。
var uidGenerator = func() int64 { return uidbuilder.GenerateUID() }

func (self *Player) InitData(account string) {
	self.Account = account
	self.UID = uidGenerator()
}

// IsActive 报告玩家会话是否仍有效（未被顶替、未下线）。
func (self *Player) IsActive() bool {
	return self.active
}

// deactivate 结束会话：标记失效并解绑连接，杜绝旧连接再发消息影响新会话。
func (self *Player) deactivate() {
	self.active = false
	self.agent = nil
}

func (self *Player) CallClientFunc(ret int, cmd string, ans interface{}) {
	if self.agent == nil {
		log.Error("call client on inactive player:", self.objid)
		return
	}
	errmsg := ""

	message := &msg.RetMsg{ret, errmsg, cmd, ans}
	self.agent.WriteMsg(message)
}

func (self *Player) Test(req *msg.TestReq) {
	// 业务方法在会话失效后不应再被执行；消息入口已先做拦截，
	// 这里再做一道防御，保证迟到消息不会改动玩家数据。
	if !self.active {
		return
	}
	self.testCalled++
}

// testCalled 记录 Test 业务方法被有效调用的次数，供测试与诊断使用。
func (self *Player) testCallCount() int { return self.testCalled }

// 登陆
func (self *Player) OnLogin() {
}

// OnLogout 玩家下线的生命周期回调：先停定时器，再保存数据，最后标记失效。
//
// 注意：正常断线/退出流程由 session.Manager.OnConnClosed 统一编排
// （停定时器 -> Store.Save -> 摘除会话），业务代码不要直接调用本方法，
// 否则会造成重复保存。本方法保留给需要“立即下线并落库”的特殊业务场景。
func (self *Player) OnLogout() {
	TimerManager.RmvAllTimer(self)
	self.Save()
	self.deactivate()
}

// 保存玩家数据（异步）
func (self *Player) Save() {
	playerStore.Write(self)
}

// 同步保存玩家数据，返回错误供调用方观察，不得静默丢弃。
func (self *Player) SaveSync() error {
	return playerStore.WriteSync(self)
}
