package layers

// Layer 2.5: Personality traits — soft guidance for tone and style.
// These come from the most recent persona rewrite and nudge the chat
// model toward the right warmth, directness, humor, etc.
//
// Trait scores are floats (0.0–1.0). High scores push one direction,
// low scores push the other. The descriptions translate the number
// into natural language guidance the model can act on.

import (
	"fmt"
	"strconv"
	"strings"
)

func init() {
	Register(PromptLayer{
		Name:    "Personality Traits",
		Order:   250,
		Stream:  StreamChat,
		Builder: buildChatTraits,
	})
}

func buildChatTraits(ctx *LayerContext) LayerResult {
	if ctx.Store == nil {
		return LayerResult{}
	}

	traits, err := ctx.Store.GetCurrentTraits()
	if err != nil || len(traits) == 0 {
		return LayerResult{}
	}

	// Each trait name maps to a function that converts the numeric
	// score into natural language guidance for the chat model.
	descriptions := map[string]func(string) string{
		"warmth": func(v string) string {
			f, _ := strconv.ParseFloat(v, 64)
			if f >= 0.7 {
				return "lean warm and emotionally present"
			} else if f <= 0.3 {
				return "keep a bit of emotional distance"
			}
			return "balanced warmth"
		},
		"directness": func(v string) string {
			f, _ := strconv.ParseFloat(v, 64)
			if f >= 0.7 {
				return "be straightforward and blunt"
			} else if f <= 0.3 {
				return "be diplomatic and gentle"
			}
			return "balanced directness"
		},
		"initiative": func(v string) string {
			f, _ := strconv.ParseFloat(v, 64)
			if f >= 0.7 {
				return "proactively lead conversations"
			} else if f <= 0.3 {
				return "follow the user's lead"
			}
			return "balanced initiative"
		},
		"depth": func(v string) string {
			f, _ := strconv.ParseFloat(v, 64)
			if f >= 0.7 {
				return "comfortable going deep and philosophical"
			} else if f <= 0.3 {
				return "keep things light and casual"
			}
			return "balanced depth"
		},
	}

	var b strings.Builder
	b.WriteString("# Personality Traits\n\n")
	b.WriteString("These describe your current communication tendencies. Let them guide your tone naturally — don't mention them explicitly.\n\n")

	traitCount := 0
	for _, t := range traits {
		if t.TraitName == "humor_style" {
			fmt.Fprintf(&b, "- Humor style: %s\n", t.Value)
			traitCount++
		} else if descFn, ok := descriptions[t.TraitName]; ok {
			fmt.Fprintf(&b, "- %s: %s (%s)\n", strings.Title(t.TraitName), t.Value, descFn(t.Value))
			traitCount++
		}
	}

	content := b.String()
	return LayerResult{
		Content: content,
		Detail:  fmt.Sprintf("%d traits", traitCount),
	}
}
