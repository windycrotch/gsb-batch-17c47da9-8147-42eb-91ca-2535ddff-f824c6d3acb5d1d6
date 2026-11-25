package internal

import (
	"leaf_server/base"
	"leaf_server/msg"
	"leaf_server/oss"
	"leaf_server/session"

	"github.com/name5566/leaf/gate"
	"github.com/name5566/leaf/log"
)

// SessionMgr 统一管理每个账号的在线会话，保证任一时刻同一账号
// 至多只有一个有效在线会话。
var SessionMgr = session.NewManager(&dbStore{})

func init() {
	skeleton.RegisterChanRPC("NewAgent", rpcNewAgent)
	skeleton.RegisterChanRPC("CloseAgent", rpcCloseAgent)
	skeleton.RegisterChanRPC("LoginSuccess", rpcLoginSuccess)
}

func rpcNewAgent(args []interface{}) {
	a := args[0].(gate.Agent)
	log.Debug("new agent")
	_ = a
}

// rpcCloseAgent 处理连接关闭（主动退出或网络断线）。
//
// 只有当该连接仍是账号当前有效会话时才执行下线清理；
// 已被顶替的旧连接关闭时直接忽略，绝不能把新会话踢下线。
// 清理顺序：摘除会话 -> 停定时器 -> 保存数据（由 session.Manager 编排）。
func rpcCloseAgent(args []interface{}) {
	a := args[0].(gate.Agent)
	log.Debug("close agent")

	userdata := a.UserData()
	if nil == userdata {
		// 从未登录成功的连接，直接关闭即可。
		return
	}

	info := userdata.(*base.AccountInfo)
	conn := &agentConn{agent: a}

	closed := SessionMgr.OnConnClosed(info.ObjID, conn, func(s *session.Session) {
		// 保存之前先停掉该玩家的全部定时器，
		// 保证落库期间不会有定时器继续修改玩家数据。
		if player, ok := s.Data.(*Player); ok {
			TimerManager.RmvAllTimer(player)
		}
	})
	if closed == nil {
		// 旧连接：它持有的会话已被新登录顶替，什么都不用做，
		// 绝不能影响新会话的在线状态、定时器与数据。
		log.Debug("stale agent closed, ignore:", info.ObjID)
		return
	}

	// OnConnClosed 已通过 dbStore.Save 完成数据落库（先停定时器后保存）。
	if player, ok := closed.Data.(*Player); ok {
		player.deactivate()
		PlayerManager.DelPlayer(info.ObjID)
	}
}

// rpcLoginSuccess 登录成功后由 login 模块异步调用。
//
// 流程：
//  1. BeginLogin 立即抢占账号的在线身份：若旧会话存在，旧连接收到明确的
//     下线通知并被关闭，旧会话不再处理业务消息；加载期间重复登录也只会
//     留下最新这一个占位会话；
//  2. 异步加载玩家数据，加载回调中 FinishLogin 校验本次登录是否仍有效；
//  3. 被更新的登录顶替时（ErrStaleLogin）直接放弃本次结果。
func rpcLoginSuccess(args []interface{}) {
	a := args[0].(gate.Agent)
	userdata := a.UserData()
	info := userdata.(*base.AccountInfo)

	conn := &agentConn{agent: a}
	// 先捕获旧会话：BeginLogin 会立即抢占账号的在线身份，
	// 旧会话（若存在）随后收到明确的下线通知并被关闭。
	oldSession := SessionMgr.Current(info.ObjID)
	pending := SessionMgr.BeginLogin(info.ObjID, conn)
	if oldSession != nil {
		if oldPlayer, ok := oldSession.Data.(*Player); ok {
			// 旧会话被顶替：立即停掉它的全部定时器并解绑连接，
			// 避免旧玩家对象上残留的定时器继续触发、与新会话的数据相互干扰。
			TimerManager.RmvAllTimer(oldPlayer)
			oldPlayer.deactivate()
			// 旧玩家内存中的最新进度必须先落库，否则新会话随后从库加载
			// 时只会读到上一次存档，造成切换时进度丢失。
			// 加载中（Data 为 nil）的会话没有可信快照，不保存。
			if !oldSession.Loading() {
				oldPlayer.Save()
			}
		}
		PlayerManager.DelPlayer(info.ObjID)
	}
	log.Debug("login takes over online identity:", info.ObjID, " gen:", pending.Gen())

	// 数据加载走 Store 抽象：生产环境由 dbStore 转发到 mongodb，
	// 测试环境可注入本地可控的假存储复现完整时序。
	SessionStore.Load(info.ObjID, func(param interface{}, err error) {
		if err != nil {
			// FinishLogin 负责在会话仍有效时关闭连接。
			SessionMgr.FinishLogin(pending, nil, err)
			log.Error("load player data failed:", info.ObjID, " ", err.Error())
			a.WriteMsg(&msg.RetMsg{Code: -1, Cmd: "login"})
			return
		}

		_, ferr := SessionMgr.FinishLogin(pending, param, nil)
		if ferr == session.ErrStaleLogin {
			// 数据加载完成前同账号又登录了，本次登录已被顶替，结果作废。
			log.Debug("login superseded before data loaded, drop:", info.ObjID)
			return
		}
		if ferr != nil {
			log.Error("finish login failed:", info.ObjID, " ", ferr.Error())
			return
		}

		player := param.(*Player)
		player.objid = info.ObjID
		player.agent = a
		player.active = true
		if player.Account == "" { //保存新玩家数据
			player.InitData(info.Account)
			player.Save()
			oss.ActionLog(player.objid, player.UID, oss.BILLID_REGISTER, nil)
		}

		player.OnLogin()
		PlayerManager.AddPlayer(player)

		player.CallClientFunc(0, "login", &msg.LoginAns{info.ObjID})
		oss.ActionLog(player.objid, player.UID, oss.BILLID_LOGIN, nil)
	})
}
