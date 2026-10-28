"""Let Yandex IO handle "пауза" locally for any audio source.

user_config.dat is protobuf: field 1 = message {1: id, 2: JSON config, 3: JSON}, then fields 2, 3.
We replace one substring inside the JSON and re-encode the two enclosing lengths.
Usage: patch_user_config.py IN OUT
"""
import sys

OLD = b'"personal_assistant.scenarios.player.pause":["alarm","music","bluetooth"]'
NEW = b'"personal_assistant.scenarios.player.pause":[]'


def varint(b, i):
    r = s = 0
    while True:
        c = b[i]
        i += 1
        r |= (c & 0x7F) << s
        s += 7
        if c < 0x80:
            return r, i


def enc_varint(n):
    out = bytearray()
    while True:
        c = n & 0x7F
        n >>= 7
        out.append(c | (0x80 if n else 0))
        if not n:
            return bytes(out)


def fields(b):
    """List of (tag_bytes, payload_or_None, raw_bytes) for length-delimited/varint fields."""
    i, out = 0, []
    while i < len(b):
        st = i
        t, i = varint(b, i)
        w = t & 7
        if w == 0:
            _, i = varint(b, i)
            out.append((t, None, b[st:i]))
        elif w == 2:
            ln, j = varint(b, i)
            out.append((t, b[j:j + ln], None))
            i = j + ln
        else:
            raise ValueError(f"unexpected wire type {w} at {st}")
    return out


def encode(fs):
    out = bytearray()
    for t, payload, raw in fs:
        if payload is None:
            out += raw
        else:
            out += enc_varint(t) + enc_varint(len(payload)) + payload
    return bytes(out)


def main():
    src = open(sys.argv[1], "rb").read()
    top = fields(src)
    inner = fields(top[0][1])
    cfg = inner[1][1]
    if cfg.count(OLD) != 1:
        sys.exit(f"expected exactly one pause entry, found {cfg.count(OLD)}")
    inner[1] = (inner[1][0], cfg.replace(OLD, NEW), None)
    top[0] = (top[0][0], encode(inner), None)
    out = encode(top)
    # sanity: unchanged parts re-encode identically
    assert encode(fields(src)) == src
    open(sys.argv[2], "wb").write(out)
    print(f"ok: {len(src)} -> {len(out)} bytes")


main()
