package gameapi

import "github.com/pkg/errors"

var (
	ErrNoImplementNetwork = errors.New("no implement network: ")
	ErrConnClosing        = errors.New("session connect is closing")
	ErrUserNoIn           = errors.New("user no in instance")
	ErrWrongOpCode        = errors.New("wrong opcode")
	ErrNilPacket          = errors.New("packet is nil")
)
