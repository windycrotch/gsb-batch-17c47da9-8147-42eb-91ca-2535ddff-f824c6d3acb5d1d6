package internal

import (
	"reflect"
	"runtime/debug"

	"leaf_server/base"
	"leaf_server/msg"

	"github.com/goinggo/mapstructure"
	"github.com/name5566/leaf/gate"
	"github.com/name5566/leaf/log"
)

func init() {
	handler(&msg.GameMsg{}, onClientMsg)
}

func handler(m interface{}, h interface{}) {
	skeleton.RegisterChanRPC(reflect.TypeOf(m), h)
}

func onClientMsg(args []interface{}) {
	defer func() {
		if r := recover(); r != nil {
			log.Error("", r)
			debug.PrintStack()
		}
	}()

	m := args[0].(*msg.GameMsg)
	a := args[1].(gate.Agent)

	udata := a.UserData()
	if nil == udata {
		log.Error("please login first")
		return
	}

	account := udata.(*base.AccountInfo)
	// 被顶替或已下线的连接不得再处理该玩家的业务消息，
	// 防止旧连接迟到的消息改动新会话的数据。
	if !SessionMgr.IsCurrent(account.ObjID, &agentConn{agent: a}) {
		log.Error("agent is no longer the active session:", account.Account, " ", account.ObjID)
		return
	}

	player := PlayerManager.Get(account.ObjID)
	if nil == player || !player.IsActive() {
		log.Error("player isn't online:", account.Account, " ", account.ObjID)
		return
	}

	vplayer := reflect.ValueOf(player)
	method := vplayer.MethodByName(m.Cmd)
	if !method.IsValid() {
		log.Error("invailed player function:", m.Cmd, " ", account.ObjID)
		return
	}

	param := reflect.New(method.Type().In(0).Elem()).Interface()
	err := mapstructure.Decode(m.Req, param)
	if nil != err {
		log.Error("decode player func inparam failed:", m.Cmd, " ", account.ObjID)
		return
	}

	params := make([]reflect.Value, 1)
	params[0] = reflect.ValueOf(param)
	method.Call(params)
}
