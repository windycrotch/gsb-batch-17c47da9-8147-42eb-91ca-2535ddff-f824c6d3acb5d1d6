# Go HLS Transcoder

[![GoDoc](https://godoc.org/github.com/rendyfebry/go-hls-transcoder?status.svg)](https://godoc.org/github.com/rendyfebry/go-hls-transcoder)

Simple golang HLS transcoder with ffmpeg, exposed as **awaitable, cancellable,
success-or-failure tasks**.

## Prerequisite

- Go 1.22+
- ffmpeg

## Concepts

A transcode call is a `Task`. Submitting it returns immediately; the task runs
one ffmpeg process per requested resolution and gives you:

- `Wait()` — block until **every** requested variant has actually finished
- `Cancel()` — terminate and reap the running ffmpeg process(es); no orphan
  process is left writing segments
- `Status()` / `Done()` — observe the lifecycle (`running`, `completed`,
  `failed`, `canceled`)
- a typed failure: any single variant failing fails the whole task

Guarantees:

- **One task per target directory.** A concurrent submission to the same
  directory is rejected with `ErrTargetBusy`; two tasks can never interleave
  writes to the same segment/playlist names. Different directories run
  concurrently and canceling one never affects the others.
- **No artifacts on failure or cancellation.** All segments and variant
  playlists are first written to a unique staging directory inside the target.
  They are moved into place only after every variant succeeds, and the master
  playlist is written last, atomically. A failed or canceled task removes
  everything it produced; pre-existing files in the target directory are
  never touched.
- **Master playlist only when complete.** The master playlist references
  exactly the requested variants and is created only when all of them exist.
- **Known resolutions only.** `360p`, `480p`, `720p`, `1080p`; an unknown
  resolution is an error, never silently skipped.

## Examples

Submit a multi-bitrate task and await its result:

```go
package main

import (
	"context"
	"log"

	hls "github.com/rendyfebry/go-hls-transcoder"
)

func main() {
	task, err := hls.Transcode(context.Background(), hls.Config{
		FFmpegPath:    "/usr/local/bin/ffmpeg",
		SrcPath:       "/assets/raw/movie.mov",
		TargetPath:    "/assets/hls",
		Resolutions:   []string{"360p", "480p", "720p", "1080p"},
		MasterPlaylist: "playlist.m3u8", // optional, defaults to playlist.m3u8
	})
	if err != nil {
		// Nothing from this task remains in the target directory.
		log.Fatalf("transcode failed: %v", err)
	}
	log.Printf("done: %s", task.Status()) // completed
}
```

Submit without blocking and wait/cancel explicitly:

```go
task, err := hls.Submit(context.Background(), hls.Config{
	FFmpegPath:  ffmpeg,
	SrcPath:     src,
	TargetPath:  out,
	Resolutions: []string{"480p", "720p"},
})
if errors.Is(err, hls.ErrTargetBusy) {
	// Another task already owns this target directory; retry later.
}
// ...
task.Cancel()          // stop ffmpeg and remove this task's artifacts
if errors.Is(task.Wait(), hls.ErrTaskCanceled) {
	log.Println("task was canceled")
}
```

Canceling via context works the same way:

```go
ctx, cancel := context.WithCancel(context.Background())
go func() { /* call cancel() when needed */ }()

_, err := hls.Transcode(ctx, hls.Config{ /* ... */ })
if errors.Is(err, hls.ErrTaskCanceled) {
	log.Println("canceled via context")
}
```

The legacy synchronous helpers are retained and now wait for ffmpeg's real
exit status instead of returning as soon as the process starts:

```go
err := hls.GenerateHLS(ffmpeg, srcPath, targetPath, "480p")
```

## Error handling

| Error                       | Meaning                                                        |
| --------------------------- | -------------------------------------------------------------- |
| `ErrUnknownResolution`      | A requested resolution is not one of the four presets.        |
| `ErrTargetBusy`             | Another running task already owns the target directory.        |
| `ErrTaskCanceled`           | The task was canceled; ffmpeg was reaped and artifacts removed.|
| `ErrNoResolutions`          | No resolution was requested.                                   |
| `*VariantError`             | A specific variant's ffmpeg failed (wraps the ffmpeg error).   |

## License

MIT licensed. See the LICENSE.md file for details.
