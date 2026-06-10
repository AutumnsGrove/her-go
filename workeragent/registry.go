package workeragent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"her/config"

	"gopkg.in/yaml.v3"
)

// TaskType holds the loaded config for one registered task type.
// Each task type is a directory under workeragent/tasks/<name>/ with
// a prompt.md and a meta.yaml.
type TaskType struct {
	Name      string // directory name (e.g. "briefing", "research")
	ModelTier string // "low", "medium", "high" — maps to config tier
	promptDir string // absolute path to the task type's directory
}

var taskTypes = map[string]*TaskType{}

// Register adds a task type to the registry. Called by Init during
// directory scanning. Exported for tests.
func Register(t *TaskType) {
	taskTypes[t.Name] = t
}

// Lookup returns the task type for a given name, or nil if not found.
func Lookup(name string) *TaskType {
	return taskTypes[name]
}

// RegisteredTypes returns all registered task type names.
func RegisteredTypes() []string {
	names := make([]string, 0, len(taskTypes))
	for k := range taskTypes {
		names = append(names, k)
	}
	return names
}

// meta.yaml schema — kept minimal on purpose.
type taskMeta struct {
	Name      string `yaml:"name"`
	ModelTier string `yaml:"model_tier"`
}

// Init scans the tasks/ directory under rootDir/workeragent/tasks/ and
// registers every directory that contains a prompt.md and meta.yaml.
// Called once at startup from cmd/run.go.
func Init(rootDir string) error {
	tasksDir := filepath.Join(rootDir, "workeragent", "tasks")

	entries, err := os.ReadDir(tasksDir)
	if err != nil {
		if os.IsNotExist(err) {
			log.Info("no worker task types found (directory missing)")
			return nil
		}
		return fmt.Errorf("scanning worker tasks: %w", err)
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(tasksDir, entry.Name())
		promptPath := filepath.Join(dir, "prompt.md")
		metaPath := filepath.Join(dir, "meta.yaml")

		// Both files must exist.
		if _, err := os.Stat(promptPath); err != nil {
			continue
		}
		if _, err := os.Stat(metaPath); err != nil {
			continue
		}

		// Parse meta.yaml.
		metaBytes, err := os.ReadFile(metaPath)
		if err != nil {
			log.Warn("worker: skipping task type (unreadable meta.yaml)",
				"dir", entry.Name(), "err", err)
			continue
		}
		var meta taskMeta
		if err := yaml.Unmarshal(metaBytes, &meta); err != nil {
			log.Warn("worker: skipping task type (invalid meta.yaml)",
				"dir", entry.Name(), "err", err)
			continue
		}

		if meta.ModelTier == "" {
			meta.ModelTier = "low"
		}

		Register(&TaskType{
			Name:      entry.Name(),
			ModelTier: meta.ModelTier,
			promptDir: dir,
		})
		log.Info("worker: registered task type",
			"name", entry.Name(), "tier", meta.ModelTier)
	}

	log.Info("worker: task registry initialized", "types", len(taskTypes))
	return nil
}

// LoadPrompt reads prompt.md from disk and expands placeholders.
// Hot-reloadable — reads from disk every time so changes take effect
// without a restart.
func (t *TaskType) LoadPrompt(cfg *config.Config, instruction string, payload map[string]string) string {
	promptPath := filepath.Join(t.promptDir, "prompt.md")
	content, err := os.ReadFile(promptPath)
	if err != nil {
		log.Error("worker: failed to load prompt", "type", t.Name, "err", err)
		return fmt.Sprintf("You are a worker agent. Complete this task: %s", instruction)
	}

	prompt := string(content)

	// Standard identity placeholders.
	if cfg != nil {
		prompt = strings.ReplaceAll(prompt, "{{her}}", cfg.Identity.Her)
		prompt = strings.ReplaceAll(prompt, "{{user}}", cfg.Identity.User)
	}

	// Task-specific placeholders.
	prompt = strings.ReplaceAll(prompt, "{{instruction}}", instruction)
	for k, v := range payload {
		prompt = strings.ReplaceAll(prompt, "{{payload."+k+"}}", v)
	}

	return prompt
}
