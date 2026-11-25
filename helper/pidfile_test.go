package main

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestTakeOverNoFile(t *testing.T) {
	if err := TakeOver(filepath.Join(t.TempDir(), "helper.pid")); err != nil {
		t.Fatal(err)
	}
}

func TestTakeOverStalePid(t *testing.T) {
	p := filepath.Join(t.TempDir(), "helper.pid")
	os.WriteFile(p, []byte("999999"), 0o644) // beyond any plausible live pid
	if err := TakeOver(p); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatal("stale pidfile not removed")
	}
}

func TestTakeOverLiveProcess(t *testing.T) {
	cmd := startStubProcess(t, "sleep")
	p := filepath.Join(t.TempDir(), "helper.pid")
	os.WriteFile(p, []byte(strconv.Itoa(cmd.Process.Pid)), 0o644)
	if err := TakeOver(p); err != nil {
		t.Fatal(err)
	}
	// The pidfile is gone — ownership moved before any signal was sent —
	// and the previous process is dead or dying.
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatal("pidfile must be removed by TakeOver")
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(8 * time.Second):
		t.Fatal("predecessor still alive after TakeOver")
	}
}

// The pidfile must never be observed half-written: the ownership poll reads
// it constantly. Write it while a reader hammers the path; every read must
// be a complete, valid pid.
func TestWritePidfileIsAtomic(t *testing.T) {
	p := filepath.Join(t.TempDir(), "helper.pid")
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				b, err := os.ReadFile(p)
				if err != nil {
					continue
				}
				if pid, err := strconv.Atoi(string(b)); err != nil || pid <= 0 {
					t.Errorf("read a half-written pidfile: %q", b)
					return
				}
			}
		}
	}()
	for i := 0; i < 50; i++ {
		if err := WritePidfile(p); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
}
