package main

import (
	"context"
	"errors"
	"io"
	"log"
	"os"
	"os/signal"

	"github.com/hajimehoshi/go-mp3"
	"github.com/hajimehoshi/oto"
	"google.golang.org/grpc"

	"github.com/lukasmalkmus/stream/bytestream"
)

const (
	remoteAddr   = "localhost:10000"
	resourceName = "song.mp3"
)

func main() {
	// Ctrl+C / stop should cancel the stream promptly instead of leaving the
	// server pushing bytes to a player that went away.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	conn, err := grpc.Dial(remoteAddr, grpc.WithInsecure())
	if err != nil {
		log.Printf("Dial(%q): %v", remoteAddr, err)
		return
	}
	defer conn.Close()

	client := bytestream.NewClient(conn)
	reader, err := client.NewReader(ctx, resourceName)
	if err != nil {
		log.Printf("NewReader(%q): %v", resourceName, err)
		return
	}
	// Idempotent; also releases the stream on normal decode/playback end.
	defer reader.Close()

	dec, err := mp3.NewDecoder(reader)
	if err != nil {
		log.Printf("NewDecoder(): %v", err)
		return
	}

	p, err := oto.NewPlayer(dec.SampleRate(), 2, 2, bytestream.MaxBufSize)
	if err != nil {
		log.Printf("NewPlayer(): %v", err)
		return
	}
	defer p.Close()

	if _, err := io.Copy(p, dec); err != nil && !errors.Is(err, context.Canceled) {
		log.Printf("Copy(): %v", err)
	}
}
