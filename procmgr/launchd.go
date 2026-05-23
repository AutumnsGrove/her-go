package procmgr

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"
)

// LaunchdManager manages the bot as a macOS launchd service.
// It writes a .plist to ~/Library/LaunchAgents and uses the modern
// launchctl subcommands (bootstrap, bootout, kickstart) instead of
// the deprecated load/unload.
type LaunchdManager struct {
	label string // e.g., "com.mira.her-go"
}

func newLaunchd(label string) (*LaunchdManager, error) {
	return &LaunchdManager{label: label}, nil
}

func (m *LaunchdManager) Name() string { return "launchd" }

// Install generates a plist from the ServiceConfig and loads it
// into launchd. If the service is already loaded, it reloads.
func (m *LaunchdManager) Install(cfg ServiceConfig) error {
	dest, err := m.plistPath()
	if err != nil {
		return err
	}

	// Ensure logs directory exists.
	if err := os.MkdirAll(cfg.LogDir, 0o755); err != nil {
		return fmt.Errorf("creating log directory: %w", err)
	}

	data := plistData{
		Label:      m.label,
		BinaryPath: cfg.BinaryPath,
		WorkDir:    cfg.WorkDir,
		StdoutPath: filepath.Join(cfg.LogDir, "stdout.log"),
		StderrPath: filepath.Join(cfg.LogDir, "stderr.log"),
		UserName:   cfg.User,
		Path:       cfg.Path,
	}

	tmpl, err := template.New("plist").Parse(plistTemplate)
	if err != nil {
		return fmt.Errorf("parsing plist template: %w", err)
	}

	f, err := os.Create(dest)
	if err != nil {
		return fmt.Errorf("creating plist %s: %w", dest, err)
	}
	if err := tmpl.Execute(f, data); err != nil {
		f.Close()
		return fmt.Errorf("writing plist: %w", err)
	}
	f.Close()
	log.Infof("wrote plist: %s", dest)

	return m.bootstrap(dest)
}

// Uninstall stops the service and removes the plist file.
func (m *LaunchdManager) Uninstall() error {
	_ = m.Stop()
	dest, err := m.plistPath()
	if err != nil {
		return err
	}
	if err := os.Remove(dest); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing plist: %w", err)
	}
	return nil
}

// Start loads the service into launchd. If already loaded, reloads.
func (m *LaunchdManager) Start() error {
	dest, err := m.plistPath()
	if err != nil {
		return err
	}
	if _, err := os.Stat(dest); os.IsNotExist(err) {
		return fmt.Errorf("plist not found at %s — run `her setup` first", dest)
	}
	return m.bootstrap(dest)
}

// Stop unloads the service from launchd.
func (m *LaunchdManager) Stop() error {
	out, err := exec.Command("launchctl", "bootout", m.serviceTarget()).CombinedOutput()
	if err != nil {
		return fmt.Errorf("launchctl bootout: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// Restart uses launchctl kickstart -k to force-restart the service.
// The -k flag kills the existing instance; launchd's KeepAlive then
// brings it back automatically.
func (m *LaunchdManager) Restart() error {
	cmd := exec.Command("launchctl", "kickstart", "-k", m.serviceTarget())
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl kickstart: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// IsManaged checks whether this service is currently loaded in launchd
// by querying launchctl print.
func (m *LaunchdManager) IsManaged() bool {
	return exec.Command("launchctl", "print", m.serviceTarget()).Run() == nil
}

// plistPath returns ~/Library/LaunchAgents/{label}.plist.
func (m *LaunchdManager) plistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("could not determine home directory: %w", err)
	}
	return filepath.Join(home, "Library", "LaunchAgents", m.label+".plist"), nil
}

// domainTarget returns the launchd domain for the current user,
// e.g., "gui/501".
func (m *LaunchdManager) domainTarget() string {
	return fmt.Sprintf("gui/%d", os.Getuid())
}

// serviceTarget returns the fully qualified launchd service path,
// e.g., "gui/501/com.mira.her-go".
func (m *LaunchdManager) serviceTarget() string {
	return fmt.Sprintf("gui/%d/%s", os.Getuid(), m.label)
}

// bootstrap loads the plist using the modern launchctl bootstrap
// command. Handles the "already loaded" case by bootout + retry.
func (m *LaunchdManager) bootstrap(plistPath string) error {
	domain := m.domainTarget()
	out, err := exec.Command("launchctl", "bootstrap", domain, plistPath).CombinedOutput()
	if err == nil {
		return nil
	}

	outStr := strings.TrimSpace(string(out))

	// Service already loaded — bootout first, then retry.
	// macOS 13+ returns exit 5 / "Bootstrap failed: 5:", older versions
	// return exit 37 / "already loaded".
	if strings.Contains(outStr, "Bootstrap failed: 5:") ||
		strings.Contains(outStr, "37:") ||
		strings.Contains(outStr, "already loaded") {
		log.Info("service already loaded, reloading")
		_ = exec.Command("launchctl", "bootout", m.serviceTarget()).Run()
		out2, err2 := exec.Command("launchctl", "bootstrap", domain, plistPath).CombinedOutput()
		if err2 != nil {
			return fmt.Errorf("launchctl bootstrap (retry): %s", strings.TrimSpace(string(out2)))
		}
		return nil
	}

	return fmt.Errorf("launchctl bootstrap: %s", outStr)
}

// plistData holds the values injected into the plist template.
type plistData struct {
	Label      string
	BinaryPath string
	WorkDir    string
	StdoutPath string
	StderrPath string
	UserName   string
	Path       string
}

const plistTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>{{.Label}}</string>

    <key>ProgramArguments</key>
    <array>
        <string>{{.BinaryPath}}</string>
        <string>run</string>
    </array>

    <key>WorkingDirectory</key>
    <string>{{.WorkDir}}</string>

    <key>KeepAlive</key>
    <true/>

    <key>ThrottleInterval</key>
    <integer>3</integer>

    <key>StandardOutPath</key>
    <string>{{.StdoutPath}}</string>

    <key>StandardErrorPath</key>
    <string>{{.StderrPath}}</string>

    <key>EnvironmentVariables</key>
    <dict>
        <key>PATH</key>
        <string>{{.Path}}</string>
    </dict>

    <key>UserName</key>
    <string>{{.UserName}}</string>
</dict>
</plist>
`
