# yt-dlp-restream

A lightweight Go HTTP server that streams audio from YouTube and other yt-dlp supported sites to HTTP-capable audio receivers (WiiM, Sonos, etc.).

## How It Works

```
┌─────────────┐     HTTP      ┌─────────────┐    pipe     ┌─────────┐    pipe     ┌────────┐
│ HiFi        │ ──────────▶   │ yt-dlp      │ ─────────▶  │ yt-dlp  │ ─────────▶  │ ffmpeg │
│ Receiver    │ ◀──────────   │ restream    │ ◀───────────│         │ ◀───────────│        │
└─────────────┘   MP3 stream  └─────────────┘             └─────────┘             └────────┘
```

Your receiver requests a URL like:
```
http://your-server:8080/stream?url=https://www.youtube.com/watch?v=dQw4w9WgXcQ
```

The server extracts the audio using yt-dlp, converts it to MP3 via ffmpeg, and streams it back as an HTTP response.

## Features

- Streams audio from any yt-dlp supported site (YouTube, Vimeo, SoundCloud, etc.)
- Converts to MP3 for maximum receiver compatibility (WiiM, Sonos, etc.)
- Chunked transfer for long content (6h+ streams tested)
- Minimal resource usage - pipes directly, no disk storage
- Single binary with yt-dlp and ffmpeg as runtime dependencies

## Installation

### Using Docker (Recommended)

```bash
docker pull ghcr.io/alyxpink/yt-dlp-restream:latest

docker run -d \
  --name yt-dlp-restream \
  -p 8080:8080 \
  ghcr.io/alyxpink/yt-dlp-restream:latest
```

### Using Docker Compose

```bash
git clone https://github.com/AlyxPink/yt-dlp-restream.git
cd yt-dlp-restream
docker compose up -d
```

### From Source

Requirements:
- Go 1.25+
- yt-dlp installed and in PATH
- ffmpeg installed and in PATH

```bash
git clone https://github.com/AlyxPink/yt-dlp-restream.git
cd yt-dlp-restream
go build -o yt-dlp-restream .
./yt-dlp-restream
```

## Usage

### Start the Server

```bash
# Default port 8080
./yt-dlp-restream

# Custom port
./yt-dlp-restream -port 3000

# Custom yt-dlp path
./yt-dlp-restream -ytdlp /usr/local/bin/yt-dlp
```

### Stream Audio

Open in your receiver or browser:
```
http://localhost:8080/stream?url=https://www.youtube.com/watch?v=dQw4w9WgXcQ
```

### Health Check

```bash
curl http://localhost:8080/health
# Returns: ok
```

## Configuration

| Flag | Environment Variable | Default | Description |
|------|---------------------|---------|-------------|
| `-port` | `PORT` | `8080` | Server port |
| `-ytdlp` | `YTDLP_PATH` | `yt-dlp` | Path to yt-dlp binary |

## API Endpoints

### `GET /stream`

Streams audio from the specified URL.

**Query Parameters:**
- `url` (required): The video/audio URL to stream

**Response:**
- `Content-Type: audio/mpeg`
- Chunked transfer encoding

**Example:**
```bash
curl "http://localhost:8080/stream?url=https://www.youtube.com/watch?v=dQw4w9WgXcQ" -o audio.mp3
```

### `GET /health`

Health check endpoint.

**Response:**
- `200 OK` with body `ok`

## Supported Sites

Any site supported by yt-dlp. See the full list:
```bash
yt-dlp --list-extractors
```

Popular sites include:
- YouTube
- Vimeo
- SoundCloud
- Bandcamp
- Twitch
- And many more...

## Development

### Local Development with Docker Compose

```bash
# Start with hot reload (rebuilds on changes)
docker compose up --build

# View logs
docker compose logs -f

# Stop
docker compose down
```

### Running Tests

```bash
# Build and test
go build ./...
go vet ./...

# Manual test
./yt-dlp-restream &
curl "http://localhost:8080/stream?url=https://www.youtube.com/watch?v=dQw4w9WgXcQ" -o test.mp3
file test.mp3  # Should show: Audio file with ID3 version 2.4.0, contains: MPEG ADTS, layer III
```

## Troubleshooting

### No audio / empty response

- Check if yt-dlp is installed: `yt-dlp --version`
- Check if ffmpeg is installed: `ffmpeg -version`
- Verify the URL works with yt-dlp directly: `yt-dlp -f bestaudio "URL"`
- Check server logs for errors

### Receiver can't play the stream

- The stream is MP3 format - ensure your receiver supports MP3
- Try accessing the stream URL in a browser first
- Check server logs for ffmpeg errors

### Stream starts but stops

- Long streams may timeout - this is normal if the receiver disconnects
- Check server logs for "Stream interrupted" messages

## License

AGPL-3.0 License - see [LICENSE](LICENSE) for details.

## Acknowledgments

- [yt-dlp](https://github.com/yt-dlp/yt-dlp) - The fantastic tool that makes this possible
