# go-aac

<!-- badges:start -->
[![GitHub stars](https://img.shields.io/github/stars/skrashevich/go-aac?style=flat-square)](https://github.com/skrashevich/go-aac/stargazers)
[![Last commit](https://img.shields.io/github/last-commit/skrashevich/go-aac?style=flat-square)](https://github.com/skrashevich/go-aac/commits/main)
[![License](https://img.shields.io/github/license/skrashevich/go-aac?style=flat-square)](https://github.com/skrashevich/go-aac/blob/main/LICENSE)
<!-- badges:end -->


<div align="center">

[![stars](https://img.shields.io/github/stars/skrashevich/go-aac?style=flat-square&logo=github)](https://github.com/skrashevich/go-aac/stargazers)
[![releases](https://img.shields.io/github/downloads/skrashevich/go-aac/total?color=blue&style=flat-square&logo=github)](https://github.com/skrashevich/go-aac/releases)
[![goreport](https://goreportcard.com/badge/github.com/skrashevich/go-aac)](https://goreportcard.com/report/github.com/skrashevich/go-aac)
[![Go Reference](https://pkg.go.dev/badge/github.com/skrashevich/go-aac.svg)](https://pkg.go.dev/github.com/skrashevich/go-aac)
[![Ask DeepWiki](https://deepwiki.com/badge.svg)](https://deepwiki.com/skrashevich/go-aac)

A pure Go implementation of an AAC (Advanced Audio Codec) decoder supporting ADTS format.

</div>


## Features

- Pure Go implementation with no external dependencies
- Supports AAC-LC (Low Complexity) profile
- ADTS (Audio Data Transport Stream) format parsing
- Decodes AAC frames to PCM audio
- Full implementation of AAC decoding pipeline including:
  - Huffman decoding
  - Inverse quantization
  - Temporal Noise Shaping (TNS)
  - Mid/Side stereo
  - Intensity stereo
  - Channel coupling
  - Modified Discrete Cosine Transform (MDCT)
  - Filterbank processing

## Installation

```bash
go get github.com/skrashevich/go-aac
```

## Command-Line Tool

The repository includes a command-line tool for converting AAC files to WAV or raw PCM:

```bash
go install github.com/skrashevich/go-aac/cmd/aac2pcm@latest
```

### Usage

```bash
# Convert AAC to WAV
aac2pcm input.aac output.wav

# Output raw PCM to stdout
aac2pcm --raw input.aac > output.pcm

# Verbose mode with details
aac2pcm -v input.aac output.wav
```

Corrupt frames are skipped automatically: the tool keeps decoding after a bad
frame and writes the WAV from the remaining valid audio. In `-v` mode it reports
how many frames were skipped and the file offset of the first one. If the input
contains no decodable frames at all the tool exits non-zero and writes no output
file.

The WAV sample rate, channel count and data length always describe the samples
that were actually written, never the values declared by a corrupt or first
frame.

See [cmd/aac2pcm/main.go](cmd/aac2pcm/main.go) for a complete example of how to use the library.

## Library Usage

The library provides a simple API for decoding AAC audio. See the [aac2pcm implementation](cmd/aac2pcm/main.go) for a practical example demonstrating:

1. Probing ADTS data with `adts.Probe()`
2. Creating a decoder with `decoder.New()`
3. Reading ADTS headers with `adts.ReadHeaderFromBytes()`
4. Decoding frames with `decoder.DecodeFrame()`
5. Converting float32 samples to int16 PCM
6. Writing WAV files

### Handling corrupt frames

`DecodeFrame` distinguishes two kinds of failure:

- **Corrupt frame** — the returned error matches `decoder.ErrInvalidFrame`. The
  decoder's configuration and filter state are left untouched, so the caller may
  skip the frame and keep decoding:

  ```go
  samples, err := dec.DecodeFrame(frame)
  if errors.Is(err, decoder.ErrInvalidFrame) {
      // skip this frame, continue with the next one
      return
  }
  ```

- **Configuration error** — anything else (unsupported profile, invalid channel
  configuration, decoder not configured, ...). These are fatal and latched: every
  later call returns the same error until a successful `SetASC`, so skipping a
  frame can never make a mis-configured decoder silently recover.


## Architecture

The decoder is organized into the following packages:

- **decoder**: High-level AAC decoder interface
- **adts**: ADTS header parsing and validation
- **ics**: Individual Channel Stream processing
- **cpe**: Channel Pair Element (stereo channels)
- **cce**: Coupling Channel Element
- **huffman**: Huffman decoding with spectral tables
- **filterbank**: IMDCT filterbank processing
- **mdct**: Modified Discrete Cosine Transform
- **fft**: Fast Fourier Transform
- **tns**: Temporal Noise Shaping
- **tables**: Lookup tables for AAC decoding

## Supported Profiles

- AAC Main (partially)
- AAC-LC (Low Complexity) - **recommended**
- AAC-LTP (Long Term Prediction) (partially)

## Limitations

- SBR (Spectral Band Replication) / HE-AAC is not supported
- AAC Main and LTP prediction features are not fully implemented
- PCE (Program Config Element) is not implemented
- Gain control is not implemented

## License

This project is licensed under the LGPL v3 license, matching the original AAC.js implementation.

## Credits

- Author: Sergei Krashevich
- Based on [AAC.js](https://github.com/audiocogs/aac.js) by Devon Govett and the Audiocogs team

## Contributing

Contributions are welcome! Please feel free to submit issues or pull requests.
