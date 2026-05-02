package cmd

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"her/config"
)

// webhookPreflight runs before deployWebhook to ensure all dependencies
// are installed and configured. Returns nil when everything is ready,
// or an error if the user needs to take manual action (with instructions
// already printed).
//
// The goal: someone who has never used Cloudflare, wrangler, or
// cloudflared should be able to get webhook mode working by following
// the prompts here. No prior knowledge required.
func webhookPreflight(cfg *config.Config) error {
	log.Info("checking webhook prerequisites...")

	// ─── Step 1: Node.js / npx ───────────────────────────────────────────
	if _, err := exec.LookPath("npx"); err != nil {
		printMissing("npx (Node.js)",
			"Wrangler (the Cloudflare CLI) runs on Node.js.",
			[]string{
				"brew install node",
				"# or visit https://nodejs.org",
			},
		)
		return fmt.Errorf("npx not found — install Node.js first")
	}
	log.Info("  ✓ npx found")

	// ─── Step 2: wrangler installed ──────────────────────────────────────
	// We use npx wrangler which auto-downloads it, but check it works.
	if err := checkWranglerInstalled(); err != nil {
		printMissing("wrangler",
			"Wrangler deploys and manages Cloudflare Workers.",
			[]string{
				"npm install -g wrangler",
				"# or it will auto-install via npx on first use",
			},
		)
		return fmt.Errorf("wrangler not working: %w", err)
	}
	log.Info("  ✓ wrangler available")

	// ─── Step 3: wrangler authenticated ──────────────────────────────────
	if err := checkWranglerAuth(); err != nil {
		printAction("wrangler login",
			"Wrangler needs to be logged in to your Cloudflare account to deploy Workers.",
			"npx wrangler login",
			"This opens a browser window. Sign in (free account works) and authorize.",
		)
		return fmt.Errorf("wrangler not authenticated — run `npx wrangler login` first")
	}
	log.Info("  ✓ wrangler authenticated")

	// ─── Step 4: cloudflared installed ───────────────────────────────────
	if _, err := exec.LookPath("cloudflared"); err != nil {
		printMissing("cloudflared",
			"Cloudflare Tunnel creates a secure connection from your machine to the internet\n"+
				"  without opening ports or needing a public IP. It's free and handles TLS for you.",
			[]string{
				"brew install cloudflare/cloudflare/cloudflared",
			},
		)
		return fmt.Errorf("cloudflared not found — install it first")
	}
	log.Info("  ✓ cloudflared found")

	// ─── Step 5: cloudflared authenticated ───────────────────────────────
	if err := checkCloudflaredAuth(); err != nil {
		printAction("cloudflared login",
			"Cloudflared needs to be linked to your Cloudflare account to create tunnels.",
			"cloudflared tunnel login",
			"This opens a browser. Pick any domain (or add one — even a free .workers.dev works).",
		)
		return fmt.Errorf("cloudflared not authenticated — run `cloudflared tunnel login` first")
	}
	log.Info("  ✓ cloudflared authenticated")

	// ─── Step 6: KV namespace ────────────────────────────────────────────
	if cfg.Cloudflare.KVNamespaceID == "" {
		log.Info("  ⚠ no KV namespace configured — creating one...")
		nsID, err := createKVNamespace()
		if err != nil {
			printAction("create KV namespace",
				"The Worker needs a KV namespace to store routing state (which instance gets traffic).",
				"npx wrangler kv namespace create HER_KV",
				"Copy the 'id' value from the output into config.yaml → cloudflare.kv_namespace_id",
			)
			return fmt.Errorf("could not create KV namespace: %w", err)
		}
		cfg.Cloudflare.KVNamespaceID = nsID
		log.Info("  ✓ KV namespace created", "id", nsID)
	} else {
		log.Info("  ✓ KV namespace configured")
	}

	// ─── Step 7: Tunnel ──────────────────────────────────────────────────
	if cfg.Tunnel.Name == "" {
		printAction("create a tunnel",
			"A Cloudflare Tunnel securely connects your machine to the internet.\n"+
				"  Pick any name you like (e.g., \"her-prod\").",
			"cloudflared tunnel create her-prod",
			"Then fill in config.yaml:\n"+
				"    tunnel:\n"+
				"      name: her-prod\n"+
				"      credentials_file: ~/.cloudflared/<tunnel-id>.json\n"+
				"      domain: <your-domain>  # e.g., her.yourdomain.com\n\n"+
				"  If you don't have a domain, you can use a free one:\n"+
				"    cloudflared tunnel route dns her-prod her-<yourname>.cfargotunnel.com",
		)
		return fmt.Errorf("no tunnel configured — create one and fill in config.yaml tunnel section")
	}
	log.Info("  ✓ tunnel configured", "name", cfg.Tunnel.Name)

	// ─── Step 8: Tunnel domain ───────────────────────────────────────────
	if cfg.Tunnel.Domain == "" {
		printAction("route a domain to your tunnel",
			"Telegram needs a public URL to send updates to. The tunnel exposes your\n"+
				"  local webhook server at a domain you control.",
			fmt.Sprintf("cloudflared tunnel route dns %s <your-domain>", cfg.Tunnel.Name),
			"Then set tunnel.domain in config.yaml to that domain.",
		)
		return fmt.Errorf("no tunnel domain configured — route a domain to your tunnel")
	}
	log.Info("  ✓ tunnel domain configured", "domain", cfg.Tunnel.Domain)

	// ─── Step 9: Account ID ──────────────────────────────────────────────
	if cfg.Cloudflare.AccountID == "" {
		acctID, err := getAccountID()
		if err != nil {
			printAction("set account ID",
				"The Cloudflare account ID is needed for API calls. Find it in your\n"+
					"  Cloudflare dashboard URL: https://dash.cloudflare.com/<account-id>/...",
				"npx wrangler whoami",
				"Copy the Account ID from the table into config.yaml → cloudflare.account_id",
			)
			return fmt.Errorf("could not determine account ID: %w", err)
		}
		cfg.Cloudflare.AccountID = acctID
		log.Info("  ✓ account ID found", "id", acctID)
	} else {
		log.Info("  ✓ account ID configured")
	}

	log.Info("all webhook prerequisites satisfied ✓")
	return nil
}

// ─── Dependency checks ───────────────────────────────────────────────────────

func checkWranglerInstalled() error {
	cmd := exec.Command("npx", "wrangler", "--version")
	cmd.Dir = "worker"
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, string(out))
	}
	return nil
}

func checkWranglerAuth() error {
	cmd := exec.Command("npx", "wrangler", "whoami")
	cmd.Dir = "worker"
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("not authenticated")
	}
	// "You are logged in" or a table with account info = good.
	// "Not authenticated" or OAuth error = bad.
	output := string(out)
	if strings.Contains(output, "Not authenticated") || strings.Contains(output, "not logged in") {
		return fmt.Errorf("not authenticated")
	}
	return nil
}

func checkCloudflaredAuth() error {
	// cloudflared stores a cert.pem in ~/.cloudflared/ after login.
	// Its presence is the simplest auth check.
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	certPath := home + "/.cloudflared/cert.pem"
	if _, err := os.Stat(certPath); err != nil {
		return fmt.Errorf("no cert.pem found — not logged in")
	}
	return nil
}

// ─── Auto-creation helpers ───────────────────────────────────────────────────

// kvNamespaceIDPattern matches the ID from wrangler's KV create output.
// Output looks like: '{ "id": "abc123..." }'  or  'id = "abc123..."'
var kvNamespaceIDPattern = regexp.MustCompile(`"id"\s*[:=]\s*"([a-f0-9]{32})"`)

func createKVNamespace() (string, error) {
	cmd := exec.Command("npx", "wrangler", "kv", "namespace", "create", "HER_KV")
	cmd.Dir = "worker"
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%w: %s", err, string(out))
	}

	output := string(out)
	match := kvNamespaceIDPattern.FindStringSubmatch(output)
	if len(match) < 2 {
		// Try alternate format — newer wrangler prints a table.
		// Fall back to asking the user to paste it.
		return "", fmt.Errorf("created but could not parse ID from output:\n%s", output)
	}
	return match[1], nil
}

// accountIDPattern matches the account ID from wrangler whoami output.
var accountIDPattern = regexp.MustCompile(`([a-f0-9]{32})`)

func getAccountID() (string, error) {
	cmd := exec.Command("npx", "wrangler", "whoami")
	cmd.Dir = "worker"
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", err
	}

	// Parse the account ID from the table output.
	// The table has an "Account ID" column with a 32-char hex string.
	lines := strings.Split(string(out), "\n")
	for _, line := range lines {
		// Look for lines with a 32-char hex string that aren't the header.
		if strings.Contains(line, "Account Name") {
			continue
		}
		match := accountIDPattern.FindString(line)
		if match != "" {
			return match, nil
		}
	}
	return "", fmt.Errorf("could not parse account ID from wrangler whoami output")
}

// ─── User-facing output helpers ──────────────────────────────────────────────

// printMissing prints a clear message about a missing dependency with
// install instructions. Designed to be readable by someone who has never
// used these tools before.
func printMissing(name, explanation string, installCmds []string) {
	fmt.Printf("\n  ✗ %s is not installed\n", name)
	fmt.Printf("    %s\n\n", explanation)
	fmt.Println("    Install:")
	for _, cmd := range installCmds {
		fmt.Printf("      %s\n", cmd)
	}
	fmt.Println()
	fmt.Println("    Then re-run: her setup")
	fmt.Println()
}

// printAction prints a clear message about a manual step the user needs
// to take, with the exact command and an explanation of what it does.
func printAction(name, explanation, command, afterNote string) {
	fmt.Printf("\n  ⚠ Action needed: %s\n", name)
	fmt.Printf("    %s\n\n", explanation)
	fmt.Printf("    Run:\n      %s\n\n", command)
	if afterNote != "" {
		fmt.Printf("    %s\n", afterNote)
	}
	fmt.Println()
	fmt.Println("    Then re-run: her setup")
	fmt.Println()
}

// promptContinue asks the user if they want to continue or abort.
// Used after showing information that might make them want to stop.
func promptContinue(message string) bool {
	fmt.Printf("  %s [Y/n] ", message)
	reader := bufio.NewReader(os.Stdin)
	answer, _ := reader.ReadString('\n')
	answer = strings.TrimSpace(strings.ToLower(answer))
	return answer == "" || answer == "y" || answer == "yes"
}
