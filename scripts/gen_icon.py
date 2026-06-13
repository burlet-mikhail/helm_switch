#!/usr/bin/env python3
"""Generate a 512x512 PNG icon for Helm Switch — a ship's helm (steering
wheel) on a deep-ocean gradient. Pure stdlib: writes the PNG by hand so the
build has no Pillow/numpy dependency. Edges are smoothed with 2x2 supersampling.
"""
import math
import struct
import zlib

# Kept at the path the Makefile feeds to `sips`.
OUTPUT = "/tmp/ruswitch_icon_src.png"

SIZE = 512
C = SIZE / 2.0  # center

# Geometry (pixels, relative to center)
R_RIM_OUT = 150.0   # outer edge of the wheel rim
R_RIM_IN = 121.0    # inner edge of the rim
R_HUB = 37.0        # central hub disk
R_HUB_HOLE = 15.0   # small hole punched in the hub (shows background)
R_HANDLE = 181.0    # distance of the handle knobs from center
KNOB_R = 25.0       # radius of each handle knob
SPOKE_HALF = 12.0   # half-width of a spoke
N_SPOKES = 8

# Colors
BG_TOP = (44, 96, 140)     # lighter ocean blue (top)
BG_BOT = (18, 44, 72)      # deep navy (bottom)
FG = (248, 246, 240)       # warm off-white (the wheel)

_QUARTER = math.pi / N_SPOKES * 2  # angular spacing between spokes (pi/4)


def png_chunk(chunk_type: bytes, data: bytes) -> bytes:
    c = chunk_type + data
    return struct.pack(">I", len(data)) + c + struct.pack(">I", zlib.crc32(c) & 0xFFFFFFFF)


def lerp(a, b, t):
    return a + (b - a) * t


def bg_color(y: float):
    t = y / (SIZE - 1)
    return (
        lerp(BG_TOP[0], BG_BOT[0], t),
        lerp(BG_TOP[1], BG_BOT[1], t),
        lerp(BG_TOP[2], BG_BOT[2], t),
    )


def is_wheel(px: float, py: float) -> bool:
    """True if the sample point is part of the helm wheel."""
    dx = px - C
    dy = py - C
    dist = math.hypot(dx, dy)

    # Hub disk (with a punched hole in the very center).
    if dist <= R_HUB:
        return dist > R_HUB_HOLE

    # Rim ring.
    if R_RIM_IN <= dist <= R_RIM_OUT:
        return True

    # Spokes + handle knobs: snap to the nearest spoke direction and test it.
    ang = math.atan2(dy, dx)
    k = round(ang / _QUARTER)
    sa = k * _QUARTER
    dirx, diry = math.cos(sa), math.sin(sa)

    proj = dx * dirx + dy * diry            # distance along the spoke
    perp = abs(dx * diry - dy * dirx)       # distance across the spoke

    # Spoke shaft from the hub out to the handle.
    if 0.0 <= proj <= R_HANDLE and perp <= SPOKE_HALF:
        return True

    # Rounded handle knob at the spoke tip.
    kx, ky = R_HANDLE * dirx, R_HANDLE * diry
    if math.hypot(px - (C + kx), py - (C + ky)) <= KNOB_R:
        return True

    return False


def make_png() -> bytes:
    sig = b"\x89PNG\r\n\x1a\n"
    ihdr = png_chunk(b"IHDR", struct.pack(">IIBBBBB", SIZE, SIZE, 8, 2, 0, 0, 0))

    # 2x2 supersample offsets for anti-aliased edges.
    offsets = (0.25, 0.75)
    raw = bytearray()
    for y in range(SIZE):
        raw.append(0)  # filter type: none
        br, bg, bb = bg_color(y)
        for x in range(SIZE):
            cov = 0
            for oy in offsets:
                for ox in offsets:
                    if is_wheel(x + ox, y + oy):
                        cov += 1
            if cov == 0:
                raw += bytes((int(br + 0.5), int(bg + 0.5), int(bb + 0.5)))
            elif cov == 4:
                raw += bytes(FG)
            else:
                a = cov / 4.0
                raw += bytes((
                    int(lerp(br, FG[0], a) + 0.5),
                    int(lerp(bg, FG[1], a) + 0.5),
                    int(lerp(bb, FG[2], a) + 0.5),
                ))
    idat = png_chunk(b"IDAT", zlib.compress(bytes(raw), 9))
    iend = png_chunk(b"IEND", b"")
    return sig + ihdr + idat + iend


if __name__ == "__main__":
    data = make_png()
    with open(OUTPUT, "wb") as f:
        f.write(data)
    print(f"Icon generated: {OUTPUT} ({len(data)} bytes)")
