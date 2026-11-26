// Copyright 2024 Jelly Terra
// This Source Code Form is subject to the terms of the Mozilla Public License, v. 2.0
// that can be found in the LICENSE file and https://mozilla.org/MPL/2.0/.

package main

import (
	"bytes"
	"github.com/yutopp/go-rtmp"
	rtmpmsg "github.com/yutopp/go-rtmp/message"
	"net/url"
	"sync"
)

type Remote struct {
	label  string
	Client *rtmp.ClientConn
	Stream *rtmp.Stream

	closeOnce sync.Once
}

func (e *Remote) Label() string { return e.label }

// DialRemote establishes an outbound RTMP connection and starts publishing.
// Any failure tears down the resources created up to that point, so a failed
// dial never leaves a dangling connection behind.
func DialRemote(h *Handler, host, path, key string) (*Remote, error) {

	label := "remote:" + host + "/" + path

	up := h.ConnMsg.Command
	connMsg := rtmpmsg.NetConnectionConnectCommand{
		App:            path,
		Type:           up.Type,
		FlashVer:       up.FlashVer,
		TCURL:          "rtmp://" + host + "/" + path,
		Fpad:           up.Fpad,
		Capabilities:   up.Capabilities,
		AudioCodecs:    up.AudioCodecs,
		VideoCodecs:    up.VideoCodecs,
		VideoFunction:  up.VideoFunction,
		ObjectEncoding: up.ObjectEncoding,
	}

	u, err := url.Parse("rtmp://" + host)
	if err != nil {
		return nil, err
	}
	if u.Port() == "" {
		host += ":1935"
	}

	client, err := rtmp.Dial("rtmp", host, &rtmp.ConnConfig{})
	if err != nil {
		return nil, err
	}

	stream, err := func() (*rtmp.Stream, error) {
		if err := client.Connect(&rtmpmsg.NetConnectionConnect{
			Command: connMsg,
		}); err != nil {
			return nil, err
		}

		stream, err := client.CreateStream(&rtmpmsg.NetConnectionCreateStream{}, 128)
		if err != nil {
			return nil, err
		}

		if err := stream.Publish(&rtmpmsg.NetStreamPublish{
			CommandObject:  nil,
			PublishingName: key,
			PublishingType: h.PubMsg.PublishingType,
		}); err != nil {
			return nil, err
		}

		return stream, nil
	}()
	if err != nil {
		// Clean up the half-established outbound connection before reporting.
		_ = client.Close()
		return nil, err
	}

	return &Remote{
		label:  label,
		Client: client,
		Stream: stream,
	}, nil
}

// Close is idempotent: the outbound RTMP connection is torn down at most once.
func (e *Remote) Close() {
	e.closeOnce.Do(func() {
		// Stream.Close is currently a no-op in go-rtmp; keep it for forward
		// compatibility with library versions that implement it.
		_ = e.Stream.Close()
		_ = e.Client.Close()
	})
}

func (e *Remote) WriteAudio(timestamp uint32, payload []byte) error {
	return e.Stream.Write(5, timestamp, &rtmpmsg.AudioMessage{
		Payload: bytes.NewReader(payload),
	})
}

func (e *Remote) WriteVideo(timestamp uint32, payload []byte) error {
	return e.Stream.Write(6, timestamp, &rtmpmsg.VideoMessage{
		Payload: bytes.NewReader(payload),
	})
}

func (e *Remote) WriteSetFrame(timestamp uint32, data *rtmpmsg.DataMessage) error {
	return e.Stream.Write(8, timestamp, data)
}
