package transcoder

import (
	"reflect"
	"testing"

	"github.com/xfrr/goffmpeg/media"
)

func TestTranscoder(t *testing.T) {
	t.Run("#SetWhiteListProtocols", func(t *testing.T) {
		t.Run("Should not set -protocol_whitelist option if it isn't present", func(t *testing.T) {
			ts := Transcoder{}

			ts.SetMediaFile(&media.File{})
			if reflect.DeepEqual(ts.GetCommand()[0:2], []string{"-protocol_whitelist", "file,http,https,tcp,tls"}) {
				t.Fatal("unexpected protocol_whitelist prefix when none was configured")
			}
			for _, arg := range ts.GetCommand() {
				if arg == "protocol_whitelist" {
					t.Fatal("command must not contain protocol_whitelist when not configured")
				}
			}
		})

		t.Run("Should set -protocol_whitelist option if it's present", func(t *testing.T) {
			ts := Transcoder{}

			ts.SetMediaFile(&media.File{})
			ts.SetWhiteListProtocols([]string{"file", "http", "https", "tcp", "tls"})

			want := []string{"-protocol_whitelist", "file,http,https,tcp,tls"}
			if got := ts.GetCommand()[0:2]; !reflect.DeepEqual(got, want) {
				t.Fatalf("command prefix = %v, want %v", got, want)
			}
		})
	})
}
