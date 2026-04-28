package mood

import (
	"strings"
	"unicode"
)

// ScoreSignals returns a 0-1 heuristic score measuring how "loud"
// the mood signal is across the given turns. Independent from the
// LLM's self-rated confidence: the agent takes max(LLM, heuristic) so
// explicit affect slips through even when the model under-rates
// itself.
//
// Scoring is deliberately simple and transparent:
//   +0.50 for any first-person affect phrase ("I'm exhausted", "I feel…")
//   +0.25 for any bare affect word ("exhausted", "stressed")
//   +0.15 for an intensity modifier ("absolutely", "so", "really")
//   +0.10 for an affect-bearing emoji
//   cap at 1.0
//
// The point isn't precision — it's a second vote independent of the
// LLM, so a single-channel failure can't put a clearly-expressed mood
// below the drop threshold.
func ScoreSignals(turns []Turn) float64 {
	// Only look at the user's own words. The bot's reply doesn't
	// count (otherwise "I'm sorry you're feeling stressed" would
	// boost its own confidence).
	var b strings.Builder
	for _, t := range turns {
		if t.Role != "user" {
			continue
		}
		b.WriteString(strings.ToLower(t.ScrubbedContent))
		b.WriteByte(' ')
	}
	text := b.String()
	if text == "" {
		return 0
	}

	score := 0.0

	// First-person framings. These are the strongest signal because
	// they're self-ascription.
	for _, phrase := range firstPersonAffect {
		if strings.Contains(text, phrase) {
			score += 0.50
			break
		}
	}

	// Bare affect words anywhere. Cheap to overfit on — hence a
	// break after the first hit so we don't stack credit.
	for _, word := range affectWords {
		if containsWord(text, word) {
			score += 0.25
			break
		}
	}

	// Intensity modifiers ("absolutely", "so") boost the signal
	// only if at least one of the above already fired.
	if score > 0 {
		for _, mod := range intensityModifiers {
			if containsWord(text, mod) {
				score += 0.15
				break
			}
		}
	}

	// Emoji kick.
	if containsAffectEmoji(text) {
		score += 0.10
	}

	if score > 1.0 {
		score = 1.0
	}
	return score
}

// firstPersonAffect — phrases that signal the user is describing
// their own emotional state. Includes both first-person ("I'm", "I feel")
// and third-person emotional framing ("it feels like", "everything is")
// which people commonly use to describe their own moods indirectly.
//
// False positives are cheap (one extra LLM call); false negatives mean
// silently dropping a real mood. Err on the side of letting things through.
var firstPersonAffect = []string{
	// Direct first-person
	"i'm ",
	"i am ",
	"i feel ",
	"i'm feeling ",
	"i'm so ",
	"i'm really ",
	"im ", // common typing shortcut
	"i've been ",
	"ive been ",
	"i've felt ",
	"i was ",
	"i just feel ",
	"i can't even ",

	// Third-person emotional framing — people describe their own
	// state this way ("it feels like everything is heavy")
	"it feels ",
	"it's like ",
	"its like ",
	"everything is ",
	"everything feels ",
	"everything just ", // filler word between "everything" and "feels/is"
	"things are ",
	"things feel ",
	"just feels ",  // "everything just feels gray", "it just feels wrong"
	"just feel ",   // "i just feel like"

	// Casual / colloquial framing
	"feeling ",     // "feeling pretty good about"
	"having a ",    // "having a great day", "having a rough time"
	"been feeling ", // "been feeling off lately"
	"kinda ",       // "kinda stressed", "kinda happy"
	"lowkey ",      // "lowkey anxious", "lowkey excited"
}

// affectWords — bare emotion words likely to appear in a real mood
// expression. Balanced across the full valence spectrum: unpleasant,
// neutral, and pleasant. Cross-referenced against Apple's label list
// but broader (includes casual variants like "tired", "wiped",
// "stoked", "hyped").
var affectWords = []string{
	// Unpleasant — low valence
	"angry", "anxious", "ashamed", "awful", "bad",
	"bitter", "blue", "broken", "crushed", "dead",
	"defeated", "depressed", "destroyed", "disappointed",
	"discouraged", "down", "drained", "dread", "embarrassed",
	"empty", "exhausted", "flat", "frustrated", "furious",
	"gray", "grey", "grieving", "guilty", "heavy",
	"helpless", "hopeless", "hurt",
	"irritated", "jealous", "lonely", "lost", "miserable",
	"numb", "off", "overwhelmed", "panicked", "pissed",
	"rough", "sad", "scared", "shattered", "spooked",
	"stressed", "stuck", "sucks", "terrible", "terrified",
	"tired", "trapped", "wiped", "worried", "worthless",

	// Pleasant — high valence
	"alive", "amazing", "blessed", "blissful", "brave", "bright",
	"buzzing", "cheerful", "cozy", "delighted", "ecstatic",
	"elated", "energized", "excited", "fantastic", "free",
	"fulfilled", "giddy", "glad", "glowing", "grateful",
	"great", "happy", "hopeful", "hyped", "incredible",
	"inspired", "joyful", "loved", "motivated", "overjoyed",
	"peaceful", "proud", "pumped", "radiant", "refreshed",
	"relieved", "stoked", "thankful", "thrilled", "unreal",
	"upbeat", "vibrant", "wonderful",

	// Neutral / mixed
	"calm", "chill", "confident", "content", "fine",
	"good", "meh", "okay", "relaxed", "solid",
	"weird",
}

// intensityModifiers — adverbs/adjectives that make affect louder.
var intensityModifiers = []string{
	"absolutely", "completely", "entirely", "extremely",
	"really", "so", "super", "totally", "utterly",
	"very",
}

// containsWord checks for a whole-word match in lowercase text.
// strings.Contains is too loose ("happy" would match "unhappy"); we
// want word-boundary semantics without pulling in regexp.
func containsWord(text, word string) bool {
	idx := 0
	for {
		found := strings.Index(text[idx:], word)
		if found < 0 {
			return false
		}
		start := idx + found
		end := start + len(word)

		leftOK := start == 0 || !isWordByte(text[start-1])
		rightOK := end == len(text) || !isWordByte(text[end])
		if leftOK && rightOK {
			return true
		}
		idx = end
		if idx >= len(text) {
			return false
		}
	}
}

func isWordByte(b byte) bool {
	r := rune(b)
	return unicode.IsLetter(r) || unicode.IsDigit(r) || b == '\''
}

// containsAffectEmoji returns true when any of a short curated list
// of affect-bearing emoji appears in the text. We look at the raw
// string (emoji are multibyte UTF-8) rather than decoding every
// rune — strings.Contains handles it fine.
func containsAffectEmoji(text string) bool {
	for _, e := range affectEmoji {
		if strings.Contains(text, e) {
			return true
		}
	}
	return false
}

var affectEmoji = []string{
	"😭", "😢", "😔", "😞", "😩",   // unpleasant cluster
	"😤", "😠", "😡", "🤬",          // anger
	"😰", "😨", "😱", "😖",          // anxiety / fear
	"😊", "😄", "😃", "😁", "😆",   // happy
	"🥰", "❤️", "💕", "🙂", "😌",   // warmth / calm
	"🥳", "🎉", "✨", "💪", "🔥",   // excitement / energy
	"🤩", "😍", "💖", "🙌",          // enthusiasm / love
	"😐", "😑",                      // neutral
}
