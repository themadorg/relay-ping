#!/usr/bin/env bash
# Print hostnames of public chatmail relays listed on https://chatmail.at/relays (one per line).
set -euo pipefail
RELAYS_URL="${RELAYS_URL:-https://chatmail.at/relays}"

curl -fsSL "$RELAYS_URL" \
  | grep -oE '<a href="https://[^"]+" class="hilite"' \
  | sed -E 's/^<a href="https:\/\///; s/" class="hilite"$//; s/\/$//' \
  | sort -u
