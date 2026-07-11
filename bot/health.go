package bot

import (
	"context"
	"fmt"
	"net"
	"os"
	"sync/atomic"
	"time"
)

// HealthMonitor tracks bot health metrics and notifies systemd watchdog.
//
// This solves the "silent hang" problem where the long-polling connection
// to Telegram gets stuck without erroring. The monitor:
//
// 1. Tracks when we last received a Telegram update
// 2. Logs warnings if too much time passes without activity
// 3. Notifies systemd watchdog every 30s to prove we're alive
//
// If systemd doesn't receive a watchdog ping within WatchdogSec (configured
// in the service file), it assumes the process is hung and restarts it.
type HealthMonitor struct {
	// lastUpdateAt is a Unix timestamp (seconds since epoch) of when we
	// last received a Telegram update. Atomic because it's written from
	// message handlers and read from the health check goroutine.
	lastUpdateAt atomic.Int64

	// lastHealthLogAt tracks when we last logged health status. Used to
	// avoid spamming logs every second — we only log once per minute.
	lastHealthLogAt atomic.Int64

	// startTime records when the bot started. Used to avoid false alarms
	// during the first few minutes when activity might be low.
	startTime time.Time

	// watchdogEnabled is true if systemd watchdog is configured. Read
	// from the WATCHDOG_USEC environment variable on startup.
	watchdogEnabled bool

	// watchdogInterval is how often to notify systemd (typically half
	// the watchdog timeout). Zero if watchdog is disabled.
	watchdogInterval time.Duration
}

// NewHealthMonitor creates and initializes a health monitor.
// Call this once during bot startup.
func NewHealthMonitor() *HealthMonitor {
	h := &HealthMonitor{
		startTime: time.Now(),
	}

	// Check if systemd watchdog is enabled. systemd sets WATCHDOG_USEC
	// to the watchdog timeout in microseconds. If present, we should
	// notify systemd every N/2 microseconds to prove we're alive.
	if usec := os.Getenv("WATCHDOG_USEC"); usec != "" {
		var timeout time.Duration
		if _, err := fmt.Sscanf(usec, "%d", &timeout); err == nil {
			timeout *= time.Microsecond
			h.watchdogEnabled = true
			// Notify at half the watchdog timeout to give us a safety margin.
			h.watchdogInterval = timeout / 2
			log.Info("systemd watchdog enabled", "timeout", timeout, "notify_interval", h.watchdogInterval)
		}
	}

	// Initialize lastUpdateAt to now so we don't immediately complain
	// about no activity during the first minute after startup.
	h.lastUpdateAt.Store(time.Now().Unix())
	h.lastHealthLogAt.Store(time.Now().Unix())

	return h
}

// RecordUpdate marks that we received a Telegram update. Call this from
// every message/callback handler to keep the health monitor informed.
func (h *HealthMonitor) RecordUpdate() {
	h.lastUpdateAt.Store(time.Now().Unix())
}

// Start begins the health monitoring loop. This runs in its own goroutine
// and checks health every 30 seconds. It also notifies systemd watchdog
// if enabled.
//
// The context is used for graceful shutdown — when ctx is canceled, the
// monitor stops.
func (h *HealthMonitor) Start(ctx context.Context) {
	// Ticker fires every 30 seconds. We use this interval because:
	// - It's frequent enough to catch problems within a few minutes
	// - It's not so frequent that we spam logs or waste CPU
	// - It aligns well with typical systemd watchdog intervals (60-120s)
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Info("health monitor stopping")
			return
		case <-ticker.C:
			h.checkHealth()
			if h.watchdogEnabled {
				h.notifyWatchdog()
			}
		}
	}
}

// checkHealth examines recent activity and logs warnings if the bot appears
// stuck. This is the "health check" part of the monitor.
func (h *HealthMonitor) checkHealth() {
	now := time.Now()
	lastUpdate := time.Unix(h.lastUpdateAt.Load(), 0)
	timeSinceUpdate := now.Sub(lastUpdate)

	// Don't complain during the first 5 minutes after startup — activity
	// might be naturally low if the user isn't sending messages yet.
	if now.Sub(h.startTime) < 5*time.Minute {
		return
	}

	// Define thresholds for warnings. These are somewhat arbitrary but
	// chosen to catch real problems without false alarms:
	//
	// - 30 min: First warning (might be normal low activity)
	// - 60 min: Strong warning (something is probably wrong)
	// - 120 min: Critical warning (definitely hung)
	const (
		warnThreshold     = 30 * time.Minute
		strongWarnThresh  = 60 * time.Minute
		criticalThresh    = 120 * time.Minute
	)

	var shouldLog bool
	var level string

	switch {
	case timeSinceUpdate > criticalThresh:
		level = "CRITICAL"
		shouldLog = true
	case timeSinceUpdate > strongWarnThresh:
		level = "STRONG WARNING"
		shouldLog = true
	case timeSinceUpdate > warnThreshold:
		level = "WARNING"
		// Only log this level once per hour to avoid spam
		lastLog := time.Unix(h.lastHealthLogAt.Load(), 0)
		if now.Sub(lastLog) > time.Hour {
			shouldLog = true
		}
	}

	if shouldLog {
		log.Warn("telegram health check",
			"level", level,
			"last_update", lastUpdate.Format(time.RFC3339),
			"idle_duration", timeSinceUpdate.Round(time.Second),
			"detail", "no Telegram updates received; connection may be hung",
		)
		h.lastHealthLogAt.Store(now.Unix())
	}

	// If we're idle but not yet at warning threshold, log a heartbeat
	// once every 15 minutes so we can see the bot is alive in logs.
	if timeSinceUpdate < warnThreshold {
		lastLog := time.Unix(h.lastHealthLogAt.Load(), 0)
		if now.Sub(lastLog) > 15*time.Minute {
			log.Debug("telegram health check",
				"status", "healthy",
				"last_update", lastUpdate.Format(time.RFC3339),
				"idle_duration", timeSinceUpdate.Round(time.Second),
			)
			h.lastHealthLogAt.Store(now.Unix())
		}
	}
}

// NotifyReady tells systemd that the service has finished starting up.
// This must be called when using Type=notify in the service file, otherwise
// systemd will wait indefinitely and eventually time out.
//
// Call this once after the bot is fully initialized and ready to accept
// messages (after Start() begins listening).
func (h *HealthMonitor) NotifyReady() {
	h.sdNotify("READY=1")
	log.Info("notified systemd that service is ready")
}

// notifyWatchdog sends a keepalive notification to systemd. If we don't
// call this within the watchdog timeout, systemd will assume we're hung
// and restart the service.
//
// This uses sd_notify protocol: we write "WATCHDOG=1\n" to the Unix socket
// at $NOTIFY_SOCKET. systemd reads this and resets its watchdog timer.
func (h *HealthMonitor) notifyWatchdog() {
	h.sdNotify("WATCHDOG=1")
	log.Debug("systemd watchdog notified")
}

// sdNotify sends a notification to systemd via the sd_notify protocol.
// The message format is key=value (e.g., "READY=1" or "WATCHDOG=1").
func (h *HealthMonitor) sdNotify(state string) {
	socketPath := os.Getenv("NOTIFY_SOCKET")
	if socketPath == "" {
		// Not running under systemd with Type=notify
		return
	}

	// Connect to the systemd notify socket. This is a datagram socket
	// (like UDP) that accepts one-way messages from the service.
	//
	// The @ prefix means "abstract socket" — a Linux feature where the
	// socket name doesn't appear in the filesystem. Think of it like an
	// anonymous pipe but addressable by name.
	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{
		Name: socketPath,
		Net:  "unixgram",
	})
	if err != nil {
		log.Warn("failed to connect to systemd notify socket", "err", err)
		return
	}
	defer conn.Close()

	// Write the notification message. Format is "KEY=value\n".
	// Common messages:
	//   READY=1    - service finished starting
	//   WATCHDOG=1 - keepalive ping
	//   STATUS=... - status text shown in systemctl status
	_, err = conn.Write([]byte(state + "\n"))
	if err != nil {
		log.Warn("failed to notify systemd", "state", state, "err", err)
		return
	}
}
