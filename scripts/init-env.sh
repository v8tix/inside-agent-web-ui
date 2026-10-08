#!/usr/bin/env bash
# Creates .env from .env.example, with a generated database password and a
# generated password for the first user (admin@example.com).
#
# Usage: scripts/init-env.sh
#
# It never overwrites a .env that already has settings. The passwords are only
# written to the file; they are not printed.
set -euo pipefail
cd "$(dirname "$0")/.."
# The file holds passwords: nobody else may read it, not even while it is written.
umask 077

TARGET=".env"
TEMPLATE=".env.example"

if [ -s "$TARGET" ]; then
  echo "$TARGET already has settings; leaving it as it is."
  exit 0
fi
if [ ! -f "$TEMPLATE" ]; then
  echo "$TEMPLATE not found; run this from the repository." >&2
  exit 1
fi

# random N: N letters and digits. Every command in the pipe reads or writes a
# finite amount, so there is no SIGPIPE to trip "pipefail".
random() { head -c 96 /dev/urandom | base64 | LC_ALL=C tr -dc 'A-Za-z0-9' | cut -c "1-$1"; }

DB_PASSWORD="$(random 24)"
ADMIN_PASSWORD="$(random 20)"

sed \
  -e "s/change-me/${DB_PASSWORD}/g" \
  -e "s/^# ADMIN_EMAIL=.*/ADMIN_EMAIL=admin@example.com/" \
  -e "s/^# ADMIN_PASSWORD=.*/ADMIN_PASSWORD=${ADMIN_PASSWORD}/" \
  "$TEMPLATE" >"$TARGET"
chmod 600 "$TARGET"

echo "Wrote $TARGET with generated passwords."
echo "First user: admin@example.com; the password is ADMIN_PASSWORD in $TARGET."
