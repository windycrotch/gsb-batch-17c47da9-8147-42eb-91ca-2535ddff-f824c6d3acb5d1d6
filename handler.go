// Copyright 2024 Jelly Terra
// This Source Code Form is subject to the terms of the Mozilla Public License, v. 2.0
// that can be found in the LICENSE file and https://mozilla.org/MPL/2.0/.

package main

import (
	"bytes"
	"errors"
	"fmt"
	"github.com/yutopp/go-rtmp"
	rtmpmsg "github.com/yutopp/go-rtmp/message"
	"io"
	"net/url"
	"sync"
	"time"
)

// ErrSessionFatal marks a failure that must terminate the whole session
// (currently: a write failure of the local recording). It is distinct from a
// single-remote failure, which only removes that one endpoint.
var ErrSessionFatal = errors.New("session-level endpoint failure")

// fatalEndpoint is implemented by endpoints whose write failure must abort the
// whole session rather than be isolated. The local recording is such an
// endpoint; best-effort remote outputs are not.
type fatalEndpoint interface {
	FatalOnWriteFailure() bool
}

type Handler struct {
	rtmp.DefaultHandler

	ConnMsg *rtmpmsg.NetConnectionConnect
	PubMsg  *rtmpmsg.NetStreamPublish
	Url     *url.URL

	Time int64

	// OutDir and ProfileDir are the per-connection recording and profile
	// directories. They default to the global flag values when zero.
	OutDir     string
	ProfileDir string

	HandleFunc func(h *Handler) error

	// mu protects endpoints and every mutation driven by endpoint lifecycle.
	// The inbound RTMP message loop is single-threaded, but OnClose (client
	// disconnect) can race with the last in-flight OnAudio/OnVideo call, so all
	// access to the endpoint set goes through this lock.
	mu        sync.RWMutex
	endpoints []Endpoint

	closeOnce sync.Once

	HooksOnClose []func()
}

func (h *Handler) Logln(a ...interface{}) {
	fmt.Println(append([]any{h.Time}, a...)...)
}

// AddEndpoint registers an endpoint that was successfully created during
// session setup (an established remote or an opened recording file).
func (h *Handler) AddEndpoint(ep Endpoint) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.endpoints = append(h.endpoints, ep)
}

// snapshot returns the currently live endpoints under a read lock.
func (h *Handler) snapshot() []Endpoint {
	h.mu.RLock()
	defer h.mu.RUnlock()

	eps := make([]Endpoint, len(h.endpoints))
	copy(eps, h.endpoints)
	return eps
}

// failEndpoint permanently removes a single endpoint from the live set and
// closes it exactly once. Other endpoints are untouched and keep receiving
// media. The failure is logged with the endpoint's label so it can be traced
// back to the specific remote.
func (h *Handler) failEndpoint(ep Endpoint, stage string, err error) {
	h.mu.Lock()
	// Allocate a fresh slice rather than reusing the backing array: a
	// concurrent broadcast may already hold a snapshot copied from it, and
	// in-place filtering would mutate the elements underneath that copy.
	kept := make([]Endpoint, 0, len(h.endpoints))
	for _, e := range h.endpoints {
		if e != ep {
			kept = append(kept, e)
		}
	}
	h.endpoints = kept
	h.mu.Unlock()

	h.Logln("Endpoint", ep.Label(), stage+" failure, isolating it:", err)
	ep.Close()
}

// broadcast writes one media unit to every live endpoint. A write failure on a
// remote only isolates that remote (other outputs continue); a write failure on
// the recording is fatal for the whole session.
func (h *Handler) broadcast(write func(ep Endpoint) error) error {
	for _, ep := range h.snapshot() {
		err := write(ep)
		if err == nil {
			continue
		}

		if fe, ok := ep.(fatalEndpoint); ok && fe.FatalOnWriteFailure() {
			// A broken recording cannot be recovered: treat it as a session
			// failure so the whole session is torn down cleanly.
			h.Logln("Endpoint", ep.Label(), "write failure (session fatal):", err)
			return fmt.Errorf("%w: %s: %v", ErrSessionFatal, ep.Label(), err)
		}

		// Remote failure: drop only this endpoint and keep streaming to
		// all the others.
		h.failEndpoint(ep, "write", err)
	}

	return nil
}

func (h *Handler) OnConnect(timestamp uint32, cmd *rtmpmsg.NetConnectionConnect) (err error) {
	h.ConnMsg = cmd
	h.Url, err = url.Parse(cmd.Command.TCURL)
	if err != nil {
		return err
	}

	return nil
}

func (h *Handler) OnPublish(_ *rtmp.StreamContext, _ uint32, cmd *rtmpmsg.NetStreamPublish) error {
	h.PubMsg = cmd
	h.Time = time.Now().UnixMicro()

	if err := h.HandleFunc(h); err != nil {
		// Setup failure. Do not tear down endpoints synchronously here: we are
		// running on go-rtmp's message-loop goroutine, and a close on this
		// goroutine makes Conn.Close wait on its own writers. Returning the
		// error makes the loop exit; the server's deferred connection close
		// then invokes OnClose on the shutdown path, which closes every
		// established endpoint once and runs the afterClose webhooks.
		h.Logln("Session setup failed:", err)
		return err
	}

	h.Logln("Streaming started.")

	return nil
}

func (h *Handler) OnAudio(timestamp uint32, payload io.Reader) error {
	p, err := io.ReadAll(payload)
	if err != nil {
		return err
	}

	return h.broadcast(func(ep Endpoint) error {
		return ep.WriteAudio(timestamp, p)
	})
}

func (h *Handler) OnVideo(timestamp uint32, payload io.Reader) error {
	p, err := io.ReadAll(payload)
	if err != nil {
		return err
	}

	return h.broadcast(func(ep Endpoint) error {
		return ep.WriteVideo(timestamp, p)
	})
}

func (h *Handler) OnSetDataFrame(timestamp uint32, data *rtmpmsg.NetStreamSetDataFrame) error {
	return h.broadcast(func(ep Endpoint) error {
		return ep.WriteSetFrame(timestamp, &rtmpmsg.DataMessage{
			Name:     "@setDataFrame",
			Encoding: rtmpmsg.EncodingTypeAMF0,
			Body:     bytes.NewReader(data.Payload),
		})
	})
}

// OnClose is invoked by go-rtmp when the inbound connection ends (normal
// client disconnect or a fatal error returned from a handler). The whole
// shutdown sequence - closing every surviving endpoint once and running the
// afterClose webhooks - is guarded by closeOnce, so a duplicated OnClose (or a
// shutdown racing a disconnect) can never double-close an endpoint or fire a
// webhook twice.
func (h *Handler) OnClose() {
	h.closeOnce.Do(func() {
		eps := h.snapshot()

		h.mu.Lock()
		h.endpoints = nil
		h.mu.Unlock()

		for _, ep := range eps {
			h.Logln("Endpoint", ep.Label(), "closing.")
			ep.Close()
		}

		h.Logln("Closed.")
		for _, hook := range h.HooksOnClose {
			hook()
		}
	})
}
