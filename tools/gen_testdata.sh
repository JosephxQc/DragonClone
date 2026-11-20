#!/bin/sh
# Regenerates core/testdata/*.bin from the real Windows SDK struct layouts.
# Needs mingw-w64 (x86_64-w64-mingw32-gcc / objcopy).
set -e
cd "$(dirname "$0")"
x86_64-w64-mingw32-gcc -c layout_dump.c -o /tmp/layout_dump.o
for s in gpt mbr ext sdd geo; do
  x86_64-w64-mingw32-objcopy -O binary --only-section=.l_$s /tmp/layout_dump.o ../core/testdata/$s.bin
done
# The descriptor's strings sit after the struct; patch them in.
python3 - <<'PY'
p = '../core/testdata/sdd.bin'
b = bytearray(open(p, 'rb').read())
b += bytes(max(0, 240 - len(b)))
b[100:108] = b'SanDisk '
b[120:136] = b'Ultra           '
open(p, 'wb').write(bytes(b))
PY
