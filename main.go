// Copyright 2024 Jelly Terra
// This Source Code Form is subject to the terms of the Mozilla Public License, v. 2.0
// that can be found in the LICENSE file and https://mozilla.org/MPL/2.0/.

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"github.com/yutopp/go-rtmp"
	"io"
	"net"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

var (
	outDir     = flag.String("o", "./rec", "Streaming save directory.")
	serveAddr  = flag.String("a", ":1935", "Server listening address.")
	expireDays = flag.Int("expire", 0, "Expiration days.")
	profileDir = flag.String("p", "./profile", "Profile directory.")
)

func main() {
	flag.Parse()

	err := _main()
	if err != nil {
		fmt.Println(err)
	}
}

func _main() error {

	if *expireDays != 0 {
		go autoExpire()
	}

	l, err := net.Listen("tcp", *serveAddr)
	if err != nil {
		return err
	}

	srv := rtmp.NewServer(&rtmp.ServerConfig{
		OnConnect: func(conn net.Conn) (io.ReadWriteCloser, *rtmp.ConnConfig) {
			return conn, &rtmp.ConnConfig{
				Handler: &Handler{
					HandleFunc: handleConn,
					OutDir:     *outDir,
					ProfileDir: *profileDir,
				},

				ControlState: rtmp.StreamControlStateConfig{
					DefaultBandwidthWindowSize: 6 * 1024 * 1024 / 8,
				},
			}
		},
	})

	fmt.Println("Listen on", *serveAddr)
	err = srv.Serve(l)
	if err != nil {
		return err
	}

	return nil
}

func autoExpire() {
	for {
		err := func() error {
			entries, err := os.ReadDir(*outDir)
			if err != nil {
				return err
			}

			deadline := time.Now().AddDate(0, 0, -*expireDays)

			for _, entry := range entries {
				if entry.IsDir() {
					continue
				}

				ut, err := strconv.ParseInt(strings.Split(entry.Name(), ".")[0], 10, 64)
				if err != nil {
					return err
				}
				t := time.UnixMicro(ut)

				if t.Before(deadline) {
					fmt.Println(ut, "Expired at", t.String())
					_ = os.Remove(filepath.Join(*outDir, entry.Name()))
				}
			}

			return nil
		}()
		if err != nil {
			fmt.Println(err)
		}

		time.Sleep(time.Hour * 1)
	}
}

func handleConn(h *Handler) error {
	sp := strings.Split(h.Url.Path, "/")

	var err error

	switch sp[1] {
	case "direct":
		h.Logln("Inbound - direct forwarding.")
		err = handleDirect(h, sp[2], sp[3])
	case "record":
		h.Logln("Inbound - recording only.")
		err = handleRecord(h)
	case "profile":
		h.Logln("Inbound - profile:", sp[2])
		err = handleProfile(h, sp[2])
	default:
		err = fmt.Errorf("unexpected route mode: /%s", sp[1])
	}
	if err != nil {
		return err
	}

	h.Logln("Streaming started.")

	return nil
}

func handleDirect(h *Handler, host, path string) error {

	h.Logln("Endpoint connecting:", host)

	ep, err := DialRemote(h, host, path, h.PubMsg.PublishingName)
	if err != nil {
		h.Logln("Endpoint remote:"+host+"/"+path, "connect failure:", err)
		return err
	}

	h.AddEndpoint(ep)

	flvFile, err := CreateFlvFile(h.OutDir, fmt.Sprint(h.Time))
	if err != nil {
		return err
	}

	h.AddEndpoint(flvFile)

	return nil
}

func handleRecord(h *Handler) error {

	flvFile, err := CreateFlvFile(h.OutDir, fmt.Sprint(h.Time))
	if err != nil {
		return err
	}

	h.AddEndpoint(flvFile)

	return nil
}

func handleProfile(h *Handler, profileName string) error {

	b, err := os.ReadFile(path.Join(h.ProfileDir, profileName+".json"))
	switch {
	case err == nil:
	case os.IsNotExist(err):
		return fmt.Errorf("profile not found: %s", profileName)
	default:
		return err
	}

	var profile Profile

	err = json.Unmarshal(b, &profile)
	if err != nil {
		return err
	}

	for i, webhook := range profile.Webhooks["beforeConnect"] {
		h.Logln("Webhook beforeConnect - requesting index", i, "url:", webhook.URL)
		err := DoWebhook(h, webhook)
		if err != nil {
			// Original semantics: a beforeConnect failure aborts the session.
			// No endpoint exists yet at this point; the returned error drives
			// the normal connection shutdown (OnClose), keeping the invariant
			// that an aborted session leaves nothing open.
			h.Logln("Webhook beforeConnect - failed:", err)
			return err
		}
	}

	// Connect remotes one by one. A single remote failing to connect is
	// isolated: log it as a connection failure and continue with the remaining
	// remotes (and the recording), instead of aborting the whole session.
	connected := 0
	for i, remote := range profile.Remotes {
		h.Logln("Endpoint", i, "connecting:", remote.Host)

		ep, err := DialRemote(h, remote.Host, remote.Path, remote.Key)
		if err != nil {
			h.Logln("Endpoint remote:"+remote.Host+"/"+remote.Path, "connect failure, skipping:", err)
			continue
		}

		h.AddEndpoint(ep)
		connected++
	}

	if profile.Recording {
		flvFile, err := CreateFlvFile(h.OutDir, fmt.Sprint(h.Time))
		if err != nil {
			return err
		}
		h.AddEndpoint(flvFile)
	}

	// If every remote failed to connect and recording is not enabled, there is
	// no output at all: the session must fail explicitly rather than pretend
	// streaming started. The returned error drives OnClose, which releases any
	// endpoint that did get established.
	if connected == 0 && !profile.Recording {
		err := fmt.Errorf("all %d remote endpoint(s) failed to connect and recording is disabled", len(profile.Remotes))
		h.Logln("Session cannot start:", err)
		return err
	}

	// Register afterClose hooks before afterStart runs, so an afterStart
	// failure still has the hooks available during the resulting OnClose.
	for i, webhook := range profile.Webhooks["afterClose"] {
		h.HooksOnClose = append(h.HooksOnClose, func() {
			h.Logln("Webhook afterClose - requesting index", i, "url:", webhook.URL)
			err := DoWebhook(h, webhook)
			if err != nil {
				h.Logln("Webhook afterClose - failed:", err)
			}
		})
	}

	for i, webhook := range profile.Webhooks["afterStart"] {
		h.Logln("Webhook afterStart - requesting index", i, "url:", webhook.URL)
		err := DoWebhook(h, webhook)
		if err != nil {
			// Original semantics: an afterStart failure aborts the session.
			// Returning the error drives OnClose, which closes every connected
			// remote and the created recording (no dangling connections or
			// half-written files) and runs the afterClose webhooks.
			h.Logln("Webhook afterStart - failed:", err)
			return err
		}
	}

	return nil
}
