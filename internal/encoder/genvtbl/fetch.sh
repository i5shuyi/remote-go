#!/bin/sh
# Regenerates ../vtbl_windows.go from mingw-w64 IDL files:
#   sh internal/encoder/genvtbl/fetch.sh
set -e
here=$(cd "$(dirname "$0")" && pwd)
tmp=$(mktemp -d)
base=https://raw.githubusercontent.com/mingw-w64/mingw-w64/master/mingw-w64-headers/include
for f in d3d11.idl d3dcommon.idl dxgi.idl dxgi1_2.idl d3d10.idl mfobjects.idl mftransform.idl mfidl.idl icodecapi.idl; do
  curl -sfL -o "$tmp/$f" "$base/$f"
done
python3 "$here/genvtbl.py" "$tmp" | gofmt > "$here/../vtbl_windows.go"
rm -rf "$tmp"
