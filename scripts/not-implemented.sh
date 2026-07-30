#!/bin/sh
set -eu

target="${1:?target is required}"
phase="${2:?phase is required}"

echo "make ${target}: intentionally not implemented during phase 0; planned for ${phase}" >&2
exit 2
