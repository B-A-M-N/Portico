#!/bin/sh
# postinstall.sh - Run after package installation
set -e

# Install systemd user service for supervisor
mkdir -p /usr/lib/systemd/user
cp scripts/portico-supervisor.service /usr/lib/systemd/user/portico-supervisor.service

echo "Portico installed successfully."
echo ""
echo "To start the supervisor daemon, run:"
echo "  systemctl --user daemon-reload"
echo "  systemctl --user enable --now portico-supervisor"
echo ""
echo "Or start manually with:"
echo "  portico supervisor run"
