package msg

import (
	"leaf_server/msg/processor"
)

var Processor = processor.NewProcessor()

func init() {
	Processor.Register(&LoginMsg{})
	Processor.Register(&GameMsg{})
	Processor.Register(&RetMsg{})
	Processor.Register(&KickNotify{})
}

type LoginMsg struct {
	Cmd string      `json:"cmd"`
	Req interface{} `json:"req"`
}

type GameMsg struct {
	Cmd string      `json:"cmd"`
	Req interface{} `json:"req"`
}

type RetMsg struct {
	Code     int         `json:"code"`
	ErrorMsg string      `json:"errormsg"`
	Cmd      string      `json:"cmd"`
	Ans      interface{} `json:"data"`
}

type LoginReq struct {
	Account  string `json:"account"`
	Password string `json:"password"`
}

type LoginAns struct {
	UserCheck string `json:"usercheck"`
}

type TestReq struct {
}

type TestAns struct {
}

// KickNotify 服务器主动下发的下线通知。
// 同一账号在别处登录时，被顶替的旧连接会先收到本消息，随后连接被关闭。
type KickNotify struct {
	// Reason 下线原因，如 "replaced"（被新登录顶替）。
	Reason string `json:"reason"`
}
