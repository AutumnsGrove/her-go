package loader

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// skillProxy is the shared proxy instance, set during startup via
// SetSkillProxy. When non-nil, untrusted skills (3rd/4th party) get
// HTTP_PROXY env vars pointing here, and their domain allowlist is
// enforced. Nil means no proxy — all skills get direct access.
var skillProxy *SkillProxy

// SetSkillProxy stores the proxy instance for the runner to use.
// Called from cmd/run.go after starting the SkillProxy.
func SetSkillProxy(p *SkillProxy) {
	skillProxy = p
}

// dbProxy is the shared DB proxy instance, set during startup via
// SetDBProxy. When non-nil, skills with db permissions get DB_PROXY_URL
// env vars pointing here, and their table access is enforced.
var dbProxy *DBProxy

// SetDBProxy stores the DB proxy instance for the runner to use.
// Called from cmd/run.go after starting the DBProxy.
func SetDBProxy(p *DBProxy) {
	dbProxy = p
}

// onSkillFailed is called when a skill execution fails. The bot sets
// this during startup to emit SkillFailed events into the agent event
// channel. Nil means no event emission (skill failures are still
// returned to the caller, just not broadcast as events).
var onSkillFailed func(skillName string, errorMsg string)

// SetSkillFailedCallback stores the callback for skill failure events.
// Called from cmd/run.go after creating the bot.
func SetSkillFailedCallback(fn func(string, string)) {
	onSkillFailed = fn
}

// sidecarEmbedClient is the embedding client used for sidecar DB writes.
// Set during startup via SetEmbedClient. Nil means sidecar recording
// is disabled (no embedding model configured).
var sidecarEmbedClient embedder

// embedder is an interface for embedding text. This lets us use the real
// embed.Client in production and mock it in tests.
//
// Go interfaces are satisfied implicitly — any type that has an Embed
// method with this signature automatically implements embedder. No
// "implements" keyword needed. This is like Python's duck typing, but
// checked at compile time.
type embedder interface {
	Embed(text string) ([]float32, error)
	GetDimension() int
}

// SetEmbedClient stores the embedding client for sidecar DB writes.
// Called from cmd/run.go after creating the embed client.
func SetEmbedClient(c embedder) {
	sidecarEmbedClient = c
}

// RunResult holds the output of a skill execution.
type RunResult struct {
	Output   json.RawMessage `json:"output,omitempty"`  // parsed JSON stdout
	RawOut   string          `json:"raw_out,omitempty"` // raw stdout if not valid JSON
	Error    string          `json:"error,omitempty"`   // error message (non-zero exit, timeout, etc.)
	Duration time.Duration   `json:"duration"`          // wall-clock execution time
}

// defaultTimeout is used when the skill doesn't declare one.
const defaultTimeout = 30 * time.Second

// Run executes a skill with the given arguments. This is the core of
// run_skill — it handles compilation (Go), argument piping, timeouts,
// and output capture.
//
// The flow:
//  1. Parse the skill's timeout from permissions
//  2. For Go skills: check if binary is stale, compile if needed
//  3. Build the command (Go binary or Python via uv)
//  4. Pipe args as JSON to stdin
//  5. Run with timeout, capture stdout/stderr
//  6. Parse stdout as JSON (with raw fallback)
//
// In Python this would be subprocess.run() with stdin=PIPE and timeout.
// Go's os/exec is similar but we need to manually wire up stdin/stdout.
func Run(skill *Skill, args map[string]any) (*RunResult, error) {
	// EffectiveTimeout respects the trust tier — 2nd-party gets the full
	// declared timeout (up to 30s), while 3rd/4th party get capped shorter.
	timeout := EffectiveTimeout(skill)

	// Build the command based on language.
	var cmd *exec.Cmd
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	switch skill.Language {
	case "go":
		binPath, err := ensureGoBinary(skill)
		if err != nil {
			return &RunResult{Error: fmt.Sprintf("compilation failed: %s", err)}, nil
		}
		cmd = exec.CommandContext(ctx, binPath)

	case "python":
		// Python skills run via uv with the skill-local venv.
		// --frozen ensures no dependency updates at runtime.
		mainPy := filepath.Join(skill.Dir, "main.py")
		cmd = exec.CommandContext(ctx, "uv", "run", "--frozen", "python", mainPy)

	default:
		return nil, fmt.Errorf("unsupported language: %s", skill.Language)
	}

	// Set working directory to the skill's directory.
	cmd.Dir = skill.Dir

	// Pipe args as JSON to stdin — this is how the harness passes
	// parameters to the skill. The skill reads stdin via skillkit.ParseArgs.
	argsJSON, err := json.Marshal(args)
	if err != nil {
		return nil, fmt.Errorf("marshaling args: %w", err)
	}
	cmd.Stdin = bytes.NewReader(argsJSON)

	// Capture stdout (result) and stderr (logs) separately.
	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf

	// Pass declared env vars through. The skill only gets what it asks for
	// (plus PATH and HOME for basics). This is a minimal sandbox — the full
	// sandbox (proxy, fs restrictions) comes later.
	cmd.Env = buildSkillEnv(skill)

	// Set the proxy's domain allowlist for untrusted skills. The proxy will
	// only allow requests to domains the skill declared in permissions.domains.
	// Clear it after execution so the proxy doesn't leak restrictions to the
	// next skill run.
	if skillProxy != nil && !skill.TrustLevel.AllowDirectNetwork() {
		skillProxy.SetAllowedDomains(skill.Permissions.Domains)
		defer skillProxy.ClearAllowedDomains()
	}

	// Set the DB proxy's table permissions for skills that declared db access.
	// Same pattern as the network proxy — set before execution, clear after.
	if dbProxy != nil && skill.HasDBAccess() {
		dbProxy.SetPermissions(skill)
		defer dbProxy.ClearPermissions()

		// For 4th-party skills with db_snapshot permissions, copy declared
		// her.db tables into the sidecar before execution. The skill reads
		// from these copies, never from her.db directly.
		if skill.TrustLevel == TrustFourthParty && len(skill.Permissions.DBSnapshot) > 0 {
			if err := dbProxy.PrepareSnapshots(skill); err != nil {
				return &RunResult{Error: fmt.Sprintf("snapshot failed: %s", err)}, nil
			}
			defer dbProxy.CleanupSnapshots(skill)
		}
	}

	start := time.Now()
	err = cmd.Run()
	duration := time.Since(start)

	// Log stderr for debugging (skill's Logf output goes here).
	if stderrBuf.Len() > 0 {
		log.Debug("skill stderr", "name", skill.Name, "stderr", stderrBuf.String())
	}

	result := &RunResult{Duration: duration}

	// Handle execution errors.
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			result.Error = fmt.Sprintf("skill timed out after %s", timeout)
		} else {
			// Non-zero exit code.
			result.Error = fmt.Sprintf("skill exited with error: %s", err)
			// Still try to parse stdout — the skill might have written an
			// error JSON before exiting (via skillkit.Error).
			if stdoutBuf.Len() > 0 {
				if json.Valid(stdoutBuf.Bytes()) {
					result.Output = json.RawMessage(bytes.TrimSpace(stdoutBuf.Bytes()))
				} else {
					result.RawOut = strings.TrimSpace(stdoutBuf.String())
				}
			}
		}

		// Emit a SkillFailed event so the agent can proactively respond
		// (e.g., notify the user, suggest a fix, or retry).
		if onSkillFailed != nil {
			onSkillFailed(skill.Name, result.Error)
		}

		return result, nil
	}

	// Parse stdout as JSON.
	out := bytes.TrimSpace(stdoutBuf.Bytes())
	if len(out) == 0 {
		result.Error = "skill produced no output"
		return result, nil
	}

	if json.Valid(out) {
		result.Output = json.RawMessage(out)
	} else {
		// Not valid JSON — return as raw string with a warning.
		result.RawOut = string(out)
	}

	// Record execution in sidecar DB for 2nd-party skills (async).
	// Runs in a goroutine so it doesn't slow down the agent's response.
	// Sidecar failure is logged but never propagates — the skill result
	// is already captured and will be returned to the agent regardless.
	if sidecarEmbedClient != nil && skill.TrustLevel == TrustSecondParty {
		go recordSidecar(skill, args, result)
	}

	return result, nil
}

// ensureGoBinary compiles the Go skill if the binary is missing or stale.
// Returns the path to the binary.
//
// "Stale" means the source file (main.go) is newer than the compiled binary.
// This is like a mini Makefile — only rebuild when the source changed.
func ensureGoBinary(skill *Skill) (string, error) {
	binDir := filepath.Join(skill.Dir, "bin")
	binPath := filepath.Join(binDir, skill.Name)
	mainGo := filepath.Join(skill.Dir, "main.go")

	// Check if binary exists and is fresh.
	binInfo, binErr := os.Stat(binPath)
	srcInfo, srcErr := os.Stat(mainGo)

	if srcErr != nil {
		return "", fmt.Errorf("no main.go found in %s", skill.Dir)
	}

	// Rebuild if: binary missing OR source is newer than binary.
	needsBuild := binErr != nil || srcInfo.ModTime().After(binInfo.ModTime())

	if !needsBuild {
		return binPath, nil
	}

	log.Info("compiling skill", "name", skill.Name)

	// Ensure bin/ directory exists.
	if err := os.MkdirAll(binDir, 0755); err != nil {
		return "", fmt.Errorf("creating bin dir: %w", err)
	}

	// go build -o bin/<name> main.go
	// Use paths relative to skill.Dir since cmd.Dir is set there.
	// Using the full path (e.g., "skills/web_search/main.go") would get
	// resolved relative to cmd.Dir, doubling the prefix and causing Go
	// to interpret it as a package import path instead of a file.
	cmd := exec.Command("go", "build", "-o", filepath.Join("bin", skill.Name), "main.go")
	cmd.Dir = skill.Dir

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("go build failed: %s\n%s", err, stderr.String())
	}

	return binPath, nil
}

// parseTimeout converts a duration string like "30s" or "10s" into a
// time.Duration. Falls back to defaultTimeout on parse errors.
func parseTimeout(s string) time.Duration {
	if s == "" {
		return defaultTimeout
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return defaultTimeout
	}
	return d
}

// buildSkillEnv creates a minimal environment for the skill process.
// What a skill gets depends on its trust tier:
//
//   - 2nd-party: PATH + HOME + all declared env vars. Direct network.
//   - 3rd-party: PATH + HOME + declared env vars. Proxied network (when built).
//   - 4th-party: PATH + HOME only. No declared env vars. Proxied network.
//
// This follows the principle of least privilege — untrusted skills can't
// access API keys or secrets that might be in the parent environment.
func buildSkillEnv(skill *Skill) []string {
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
	}

	// 4th-party skills get no declared env vars — they haven't been
	// reviewed and could exfiltrate secrets via network requests.
	if skill.TrustLevel != TrustFourthParty {
		for _, key := range skill.Permissions.Env {
			if val := os.Getenv(key); val != "" {
				env = append(env, key+"="+val)
			}
		}
	}

	// Route untrusted skills through the network proxy. The proxy blocks
	// SSRF attacks (private IPs) and logs all requests. 2nd-party skills
	// (AllowDirectNetwork=true) skip this and connect directly.
	//
	// Both uppercase and lowercase variants are set because different HTTP
	// libraries check different casings: Go's net/http uses uppercase,
	// Python's urllib uses lowercase, curl checks both.
	// NO_PROXY is explicitly emptied to prevent bypass.
	if skillProxy != nil && !skill.TrustLevel.AllowDirectNetwork() {
		proxyURL := skillProxy.URL()
		env = append(env,
			"HTTP_PROXY="+proxyURL,
			"HTTPS_PROXY="+proxyURL,
			"http_proxy="+proxyURL,
			"https_proxy="+proxyURL,
			"NO_PROXY=",
			"no_proxy=",
		)
	}

	// Give skills with db permissions the DB proxy URL. The skill's
	// skillkit.DB() client reads this to connect to the proxy.
	// This is separate from HTTP_PROXY — the DB proxy listens on its own
	// port and the skill connects to it directly (not through the network proxy).
	if dbProxy != nil && skill.HasDBAccess() {
		env = append(env, "DB_PROXY_URL="+dbProxy.URL())
	}

	return env
}

// recordSidecar saves a skill execution to its sidecar database.
// Runs asynchronously (called via goroutine) — errors are logged but
// never propagated. The skill result is already captured by the time
// this runs, so sidecar failures don't affect the agent.
//
// The flow:
//  1. Build a summary text from args + result (for embedding)
//  2. Embed the summary via the embedding client
//  3. Open the sidecar DB
//  4. Record the run + embedding
//  5. Close the DB
func recordSidecar(skill *Skill, args map[string]any, result *RunResult) {
	// Build a summary to embed. Concatenate the args and a truncated
	// version of the result — this captures what was asked and what
	// came back, so KNN search works for both.
	argsJSON, _ := json.Marshal(args)
	resultPreview := ""
	if result.Output != nil {
		resultPreview = string(result.Output)
	} else if result.RawOut != "" {
		resultPreview = result.RawOut
	}
	if len(resultPreview) > 300 {
		resultPreview = resultPreview[:300]
	}
	summary := string(argsJSON) + " " + resultPreview

	// Embed the summary.
	embedding, err := sidecarEmbedClient.Embed(summary)
	if err != nil {
		log.Warn("sidecar: embedding failed", "skill", skill.Name, "error", err)
		return
	}

	// Open the sidecar DB.
	sdb, err := OpenSidecar(skill, sidecarEmbedClient.GetDimension())
	if err != nil {
		log.Warn("sidecar: open failed", "skill", skill.Name, "error", err)
		return
	}
	defer sdb.Close()

	// Record the run.
	if err := sdb.RecordRun(args, result, embedding); err != nil {
		log.Warn("sidecar: record failed", "skill", skill.Name, "error", err)
	}
}
