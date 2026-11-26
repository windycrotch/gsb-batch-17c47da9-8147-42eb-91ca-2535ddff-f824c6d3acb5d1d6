"""Generate the missing testdata/tiles.png referenced by the TMX test maps.

The TMX files declare an 112x16 px atlas of 8x8 tiles (14 columns x 2 rows).
Run: python -I make_tiles_png.py <output-path>
"""
import struct
import sys
import zlib


def make_png(width: int, height: int) -> bytes:
    # Distinct RGBA colour per 8x8 tile so frames inside the atlas differ.
    raw = bytearray()
    for y in range(height):
        raw.append(0)  # filter type 0 (none) for each scanline
        row = y // 8
        for x in range(width):
            col = x // 8
            tile = row * (width // 8) + col
            r = (tile * 37) & 0xFF
            g = (tile * 73 + 40) & 0xFF
            b = (tile * 151 + 90) & 0xFF
            raw.extend((r, g, b, 255))

    def chunk(tag: bytes, data: bytes) -> bytes:
        out = struct.pack(">I", len(data)) + tag + data
        return out + struct.pack(">I", zlib.crc32(tag + data) & 0xFFFFFFFF)

    ihdr = struct.pack(">IIBBBBB", width, height, 8, 6, 0, 0, 0)  # 8-bit RGBA
    return (
        b"\x89PNG\r\n\x1a\n"
        + chunk(b"IHDR", ihdr)
        + chunk(b"IDAT", zlib.compress(bytes(raw), 9))
        + chunk(b"IEND", b"")
    )


if __name__ == "__main__":
    out_path = sys.argv[1]
    with open(out_path, "wb") as f:
        f.write(make_png(112, 16))
    print("wrote", out_path)
