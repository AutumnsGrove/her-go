package procmgr

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"
)

// SystemdManager manages the bot as a Linux systemd service.
// It writes a .service unit file to /etc/systemd/system and uses
// systemctl for all lifecycle operations.
//
// Unlike launchd (which uses per-user agents in ~/Library), systemd
// services live in a system-wide directory and typically run as a
// dedicated user. The main concepts map directly:
//
//	launchd KeepAlive     → systemd Restart=always
//	launchd ThrottleInterval → systemd RestartSec
//	launchctl bootstrap   → systemctl enable --now
//	launchctl bootout     → systemctl disable --now
//	launchctl kickstart -k → systemctl restart
type SystemdManager struct {
	label string // e.g., "her-go"
}

func newSystemd(label string) (*SystemdManager, error) {
	return &SystemdManager{label: label}, nil
}

func (m *SystemdManager) Name() string { return "systemd" }

// Install generates a systemd unit file and enables the service.
func (m *SystemdManager) Install(cfg ServiceConfig) error {
	// Ensure logs directory exists.
	if err := os.MkdirAll(cfg.LogDir, 0o755); err != nil {
		return fmt.Errorf("creating log directory: %w", err)
	}

	data := unitData{
		Description: "her-go companion bot",
		User:        cfg.User,
		WorkDir:     cfg.WorkDir,
		BinaryPath:  cfg.BinaryPath,
		Path:        cfg.Path,
	}

	tmpl, err := template.New("unit").Parse(unitTemplate)
	if err != nil {
		return fmt.Errorf("parsing unit template: %w", err)
	}

	dest := m.unitPath()

	f, err := os.Create(dest)
	if err != nil {
		return fmt.Errorf("creating unit file %s: %w", dest, err)
	}
	if err := tmpl.Execute(f, data); err != nil {
		f.Close()
		return fmt.Errorf("writing unit file: %w", err)
	}
	f.Close()
	log.Infof("wrote unit: %s", dest)

	// Reload systemd's view of unit files, then enable + start.
	if err := m.systemctl("daemon-reload"); err != nil {
		return fmt.Errorf("daemon-reload: %w", err)
	}
	if err := m.systemctl("enable", "--now", m.label); err != nil {
		return fmt.Errorf("enable: %w", err)
	}
	return nil
}

// Uninstall stops, disables, and removes the unit file.
func (m *SystemdManager) Uninstall() error {
	_ = m.systemctl("disable", "--now", m.label)
	dest := m.unitPath()
	if err := os.Remove(dest); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing unit file: %w", err)
	}
	_ = m.systemctl("daemon-reload")
	return nil
}

func (m *SystemdManager) Start() error {
	return m.systemctl("start", m.label)
}

func (m *SystemdManager) Stop() error {
	return m.systemctl("stop", m.label)
}

func (m *SystemdManager) Restart() error {
	return m.systemctl("restart", m.label)
}

// IsManaged checks if the service is active (running) via systemctl.
func (m *SystemdManager) IsManaged() bool {
	return exec.Command("systemctl", "is-active", "--quiet", m.label).Run() == nil
}

// unitPath returns /etc/systemd/system/{label}.service.
func (m *SystemdManager) unitPath() string {
	return filepath.Join("/etc/systemd/system", m.label+".service")
}

// systemctl runs a systemctl subcommand with optional arguments.
func (m *SystemdManager) systemctl(args ...string) error {
	out, err := exec.Command("systemctl", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl %s (%s): %w", strings.Join(args, " "), strings.TrimSpace(string(out)), err)
	}
	return nil
}

type unitData struct {
	Description string
	User        string
	WorkDir     string
	BinaryPath  string
	Path        string
}

// unitTemplate is the systemd service unit file. The key settings:
//
//   - After=network-online.target: wait for network (needed for API calls)
//   - After=ollama.service: if Ollama is installed, wait for it
//   - Restart=always + RestartSec=3: same behavior as launchd's
//     KeepAlive + ThrottleInterval
//   - StandardOutput/Error=journal: logs go to journalctl (view with
//     `journalctl -u her-go -f`)
const unitTemplate = `[Unit]
Description={{.Description}}
After=network-online.target ollama.service
Wants=network-online.target

[Service]
Type=simple
User={{.User}}
WorkingDirectory={{.WorkDir}}
ExecStart={{.BinaryPath}} run
Restart=always
RestartSec=3
Environment=PATH={{.Path}}
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
`
