#!/bin/sh
# postinstall.sh - Run after package installation

set -e

# Create systemd user service directory
mkdir -p /usr/lib/systemd/user

# Install systemd user service for supervisor
cat > /usr/lib/systemd/user/portico-supervisor.service << 'EOF'
[Unit]
Description=Portico Supervisor
Documentation=https://github.com/B-A-M-N/portico
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/usr/bin/portico supervisor run
Restart=on-failure
RestartSec=5
StartLimitIntervalSec=0

# Security hardening
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=read-only
ReadWritePaths=%h/.local/state/portico %h/.local/share/portico %h/.config/portico
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
RestrictNamespaces=true
RestrictRealtime=true
LockPersonality=true
MemoryDenyWriteExecute=true

# Resource limits
LimitNOFILE=1024
LimitNPROC=512

[Install]
WantedBy=default.target
EOF

# Reload systemd user daemon
systemctl --user daemon-reload 2>/dev/null || true

# Enable and start the service (user level)
# Note: This runs in the context of the installing user
# For system-wide installs, users should run: systemctl --user enable --now portico-supervisor
echo "Portico installed successfully."
echo "To start the supervisor daemon, run:"
echo "  systemctl --user enable --now portico-supervisor"
echo ""
echo "Or start manually with:"
echo "  portico supervisor run"