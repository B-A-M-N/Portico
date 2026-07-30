#!/bin/sh
# preuninstall.sh - Run before package removal

set -e

# Stop and disable systemd user service
systemctl --user stop portico-supervisor 2>/dev/null || true
systemctl --user disable portico-supervisor 2>/dev/null || true

# Remove systemd service file
rm -f /usr/lib/systemd/user/portico-supervisor.service

# Reload systemd user daemon
systemctl --user daemon-reload 2>/dev/null || true

echo "Portico supervisor service stopped and removed."