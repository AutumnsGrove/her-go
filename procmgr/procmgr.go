// Package procmgr abstracts process supervision across platforms.
//
// On macOS, launchd manages the service via a .plist file in
// ~/Library/LaunchAgents. On Linux, systemd manages it via a .service
// unit file in /etc/systemd/system. Both keep the bot alive with
// automatic restarts, handle log routing, and provide clean
// start/stop/restart commands.
//
// Consumers (cmd/start.go, bot/handlers_admin.go, etc.) work with
// the Manager interface — they never import launchd or systemd
// directly. Think of this like Python's abc.ABC: the interface
// defines the contract, and each platform provides its own
// implementation.
package procmgr

import (
	"fmt"
	"os/user"
	"runtime"
	"strings"

	"her/logger"
)

var log = logger.WithPrefix("procmgr")

// Manager is the contract for platform-specific process supervisors.
// Both launchd (macOS) and systemd (Linux) implement this interface.
type Manager interface {
	// Install writes the service definition file (plist or unit) and
	// registers it with the supervisor. Calling Install on an already-
	// installed service updates the definition and reloads.
	Install(cfg ServiceConfig) error

	// Uninstall removes the service definition and deregisters it.
	Uninstall() error

	// Start starts the service. If the service is already running,
	// this is a no-op or returns an error depending on the platform.
	Start() error

	// Stop stops the service gracefully.
	Stop() error

	// Restart stops and restarts the service. On launchd this uses
	// kickstart -k; on systemd this uses systemctl restart.
	Restart() error

	// IsManaged returns true if the current process is running under
	// this supervisor (as opposed to foreground/manual mode).
	IsManaged() bool

	// Name returns a human-readable name for the supervisor
	// ("launchd" or "systemd").
	Name() string
}

// ServiceConfig holds the values needed to generate a service
// definition file. Platform-specific managers extract what they need.
type ServiceConfig struct {
	Label      string // service identifier (e.g., "com.mira.her-go" or "her-go")
	BinaryPath string // absolute path to the compiled binary
	WorkDir    string // working directory (where config.yaml lives)
	LogDir     string // directory for stdout/stderr logs
	User       string // OS user to run as
	Path       string // PATH environment variable snapshot
}

// ServiceLabel builds the service identifier from the bot's name.
// On macOS: "com.mira.her-go". On Linux: "her-go" (systemd convention
// doesn't use reverse-DNS).
func ServiceLabel(botName string) string {
	lower := strings.ToLower(botName)
	if runtime.GOOS == "darwin" {
		return "com." + lower + ".her-go"
	}
	return "her-go"
}

// EffectiveLabel returns the configured service label if set,
// otherwise derives one from the bot name.
func EffectiveLabel(configLabel, botName string) string {
	if configLabel != "" {
		return configLabel
	}
	return ServiceLabel(botName)
}

// New returns the platform-appropriate Manager. On macOS it returns
// a launchd manager; on Linux it returns a systemd manager. Other
// platforms get an error.
func New(label string) (Manager, error) {
	switch runtime.GOOS {
	case "darwin":
		return newLaunchd(label)
	case "linux":
		return newSystemd(label)
	default:
		return nil, fmt.Errorf("unsupported platform: %s", runtime.GOOS)
	}
}

// currentUsername returns the current OS user's name. Used by service
// configs that need to specify which user the service runs as.
func currentUsername() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return "unknown"
}
