package bytestream

// Sound-card-free verification of the production playback chain: the real
// hajimehoshi/go-mp3 decoder consumes the Reader while the stream double
// injects empty keep-alive chunks. The fixture ships with the go-mp3 module;
// the test skips when it is unavailable instead of downloading anything.

import (
	"context"
	"go/build"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/hajimehoshi/go-mp3"

	pb "github.com/lukasmalkmus/stream/proto"
)

func agentVModuleCache() string {
	if p := os.Getenv("GOMODCACHE"); p != "" {
		return p
	}
	return filepath.Join(build.Default.GOPATH, "pkg", "mod")
}

func TestAgentVPlayback_MP3DecoderThroughReaderWithEmptyChunks(t *testing.T) {
	mp3Path := filepath.Join(
		agentVModuleCache(),
		"github.com", "hajimehoshi", "go-mp3@v0.1.1",
		"example", "classic.mp3",
	)
	raw, err := os.ReadFile(mp3Path)
	if err != nil {
		t.Skipf("go-mp3 example fixture not available locally: %v", err)
	}

	const chunk = 8192
	pos := 0
	fc := &agentVFakeReadClient{}
	fc.recvFn = func(call int) (*pb.ReadResponse, error) {
		// Leading and mid-stream empty chunks must not interrupt decoding.
		if call == 0 || call == 1 || call == 7 {
			return &pb.ReadResponse{}, nil
		}
		if pos >= len(raw) {
			return nil, io.EOF
		}
		end := pos + chunk
		if end > len(raw) {
			end = len(raw)
		}
		payload := raw[pos:end]
		pos = end
		return &pb.ReadResponse{Data: payload}, nil
	}

	r := &Reader{ctx: context.Background(), readClient: fc}
	dec, err := mp3.NewDecoder(r)
	if err != nil {
		t.Fatalf("mp3.NewDecoder: %v", err)
	}
	if dec.SampleRate() <= 0 {
		t.Fatalf("invalid sample rate %d", dec.SampleRate())
	}
	if n, err := io.CopyN(io.Discard, dec, 1<<20); err != nil {
		t.Fatalf("decode through Reader failed after %d PCM bytes: %v", n, err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("Close after decode: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("idempotent Close after decode: %v", err)
	}
}
