package loader

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"her/embed"
	"her/logger"
)

// log is a prefixed logger for this package — same pattern every package
// in the codebase uses. See logger.WithPrefix.
var log = logger.WithPrefix("loader")

// Registry holds all discovered skills and their embeddings.
// It's built once at startup by scanning the skills/ directory,
// then queried by the agent via find_skill.
//
// This is like a plugin registry — it knows what's available and
// can search by intent. In Python, you'd use something like
// pkg_resources or importlib.metadata. In Go, we build it ourselves.
type Registry struct {
	mu     sync.RWMutex
	skills map[string]*Skill // keyed by skill name

	// Embeddings for KNN search. Parallel arrays — embeddings[i]
	// corresponds to names[i]. We keep them separate because the
	// embed client returns []float32 vectors, not Skill objects.
	names      []string
	embeddings [][]float32

	embedClient *embed.Client
	skillsDir   string
}

// SearchResult is returned by Find — a skill match with a similarity score.
type SearchResult struct {
	Skill *Skill
	Score float64 // cosine similarity (0.0-1.0, higher = better match)
}

// NewRegistry creates a registry that will scan the given directory for skills.
// The embed client is used to generate description embeddings for KNN search.
// Pass nil for embedClient if you don't need search (e.g., in tests).
func NewRegistry(skillsDir string, embedClient *embed.Client) *Registry {
	return &Registry{
		skills:      make(map[string]*Skill),
		embedClient: embedClient,
		skillsDir:   skillsDir,
	}
}

// Load scans the skills directory, parses each skill.md, checks requirements,
// and embeds descriptions for search. Call this at startup.
//
// Skills that don't meet requirements are logged and skipped — they won't
// appear in search results. This is intentional: the agent shouldn't know
// about skills it can't use.
//
// Returns the number of skills loaded successfully.
func (r *Registry) Load() (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Reset state — this lets Load be called again to refresh.
	r.skills = make(map[string]*Skill)
	r.names = nil
	r.embeddings = nil

	entries, err := os.ReadDir(r.skillsDir)
	if err != nil {
		return 0, fmt.Errorf("reading skills directory: %w", err)
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		// Skip non-skill directories (like skillkit/).
		skillPath := filepath.Join(r.skillsDir, entry.Name(), "skill.md")
		if _, err := os.Stat(skillPath); err != nil {
			continue // no skill.md → not a skill
		}

		skill, err := ParseSkillFile(skillPath)
		if err != nil {
			log.Warn("skipping skill", "dir", entry.Name(), "error", err)
			continue
		}

		// Check requirements — hide skills that can't run.
		if ok, reason := skill.MeetsRequirements(); !ok {
			log.Info("skill unavailable", "name", skill.Name, "reason", reason)
			continue
		}

		// Resolve trust tier from source hash verification.
		skill.TrustLevel = ResolveTrust(skill)

		r.skills[skill.Name] = skill

		// Embed the description for KNN search.
		if r.embedClient != nil {
			vec, err := r.embedClient.Embed(skill.Description)
			if err != nil {
				log.Warn("failed to embed skill description", "name", skill.Name, "error", err)
				// Skill is still usable by name, just not searchable.
			} else {
				r.names = append(r.names, skill.Name)
				r.embeddings = append(r.embeddings, vec)
			}
		}
	}

	log.Info("skills loaded", "count", len(r.skills))
	return len(r.skills), nil
}

// Get returns a skill by exact name, or nil if not found.
// This is used by run_skill when the agent already knows which skill to call.
func (r *Registry) Get(name string) *Skill {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.skills[name]
}

// List returns all registered skill names, sorted alphabetically.
func (r *Registry) List() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	names := make([]string, 0, len(r.skills))
	for name := range r.skills {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Find searches for skills matching a natural language query using
// KNN cosine similarity over description embeddings.
//
// Returns up to topK results sorted by score (highest first).
// Results below minScore are excluded.
//
// This is the core of find_skill — the agent describes what it needs
// ("get bus schedules"), and we find the closest matching skills
// by comparing embedding vectors. Same math as recall_memories.
func (r *Registry) Find(query string, topK int, minScore float64) ([]SearchResult, error) {
	if r.embedClient == nil {
		return nil, fmt.Errorf("no embedding client configured")
	}

	// Embed the query.
	queryVec, err := r.embedClient.Embed(query)
	if err != nil {
		return nil, fmt.Errorf("embedding query: %w", err)
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	// Score every skill against the query. With hundreds of skills this
	// is still instant — cosine similarity on 768-dim vectors is cheap.
	// If we ever hit thousands, we'd switch to sqlite-vec's ANN index.
	var results []SearchResult
	for i, skillVec := range r.embeddings {
		score := embed.CosineSimilarity(queryVec, skillVec)
		if score >= minScore {
			skill := r.skills[r.names[i]]
			if skill != nil {
				results = append(results, SearchResult{Skill: skill, Score: score})
			}
		}
	}

	// Sort by score descending.
	sort.Slice(results, func(i, j int) bool {
		return results[i].Score > results[j].Score
	})

	// Trim to topK.
	if len(results) > topK {
		results = results[:topK]
	}

	return results, nil
}

// Count returns the number of registered skills.
func (r *Registry) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.skills)
}
