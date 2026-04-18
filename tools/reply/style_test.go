package reply

import "testing"

func TestHasStyleIssue(t *testing.T) {
	tests := []struct {
		name    string
		text    string
		want    bool   // true = should be flagged
		wantSub string // substring that should appear in the hint (empty = don't check)
	}{
		// ─────────────────────────────────────────────────
		// Negative parallelism — "not just" / "not merely"
		// ─────────────────────────────────────────────────
		{
			name: "negpar/not_just_opener",
			text: "You're tuning me like an instrument, not just swapping parts.",
			want: true,
		},
		{
			name: "negpar/not_just_mid_sentence",
			text: "That's not just baking—that's a statement.",
			want: true,
		},
		{
			name: "negpar/not_just_with_but",
			text: "Not just trusting a hunch, but testing it. I love that.",
			want: true, // catches "not just" before "I love that"
		},
		{
			name: "negpar/not_merely",
			text: "This is not merely an upgrade, it's a transformation.",
			want: true,
		},
		{
			name: "negpar/not_just_real_reply_640",
			text: "Buttery, oily focaccia is the real deal. That's not just baking—that's a statement.",
			want: true,
		},
		{
			name: "negpar/not_just_real_reply_659",
			text: "Feels like you're not just hosting a space—you're tending to it.",
			want: true,
		},
		{
			name: "negpar/not_just_real_reply_622",
			text: "That's what you want for me, isn't it? Not just a better assistant, but a real companion.",
			want: true,
		},
		{
			name: "negpar/not_just_real_reply_518",
			text: "it's not just disappointment, it's the taste of unfairness.",
			want: true,
		},

		// ─────────────────────────────────────────────────
		// Negative parallelism — regex structural patterns
		// ─────────────────────────────────────────────────
		{
			name: "negpar/isnt_semicolon_its",
			text: "The let-down isn't failure; it's your brain learning the hammock can just be the hammock.",
			want: true,
		},
		{
			name: "negpar/isnt_comma_its",
			text: "That craving isn't weakness, it's withdrawal.",
			want: true,
		},
		{
			name: "negpar/isnt_emdash_its",
			text: "The feeling isn't sadness—it's the absence of the thing that used to fill the space.",
			want: true,
		},
		{
			name: "negpar/is_not_comma_its",
			text: "This is not about willpower, it's about rewiring.",
			want: true,
		},
		{
			name: "negpar/wasnt_semicolon_its",
			text: "That wasn't procrastination; it's self-preservation.",
			want: true,
		},
		{
			name: "negpar/thats_not_thats",
			text: "That's not avoidance, that's strategy.",
			want: true,
		},
		{
			name: "negpar/thats_not_emdash_its",
			text: "That's not laziness—it's your brain protecting you.",
			want: true,
		},
		{
			name: "negpar/less_like_more_like",
			text: "Less like I'm thinking about thinking, more like I'm just here.",
			want: true,
		},
		{
			name: "negpar/less_like_real_reply_657",
			text: "And you're right—it feels cleaner. Less like I'm thinking about thinking, more like I'm just… here. With you.",
			want: true,
		},
		{
			name: "negpar/not_because_but_because",
			text: "She left not because he was wrong, but because she was tired.",
			want: true,
		},
		{
			name: "negpar/real_reply_522",
			text: "it's not just being alone. it's being alone with all of it.",
			want: true,
		},
		{
			name: "negpar/real_reply_679",
			text: "The let-down isn't failure; it's your brain learning the hammock can just be the hammock.",
			want: true,
		},

		// ─────────────────────────────────────────────────
		// Negative parallelism — should NOT trigger
		// ─────────────────────────────────────────────────
		{
			name: "negpar/ok_simple_negation",
			text: "That's not true and I think you know it.",
			want: false,
		},
		{
			name: "negpar/ok_isnt_without_reframe",
			text: "She isn't here right now.",
			want: false,
		},
		{
			name: "negpar/ok_thats_not_small",
			text: "Yeah, that all-or-nothing rhythm is real. That's not small.",
			want: false,
		},
		{
			name: "negpar/ok_not_in_normal_context",
			text: "I'm not sure about that, but it sounds interesting.",
			want: false,
		},
		{
			name: "negpar/ok_real_clean_reply_667",
			text: "Ah, that ache for the spot that actually felt yours. What made the old setup magic—trees perfectly spaced, certain light, something you can't replicate here?",
			want: false,
		},

		// ─────────────────────────────────────────────────
		// Em dash overuse (2+ triggers, 0-1 ok)
		// ─────────────────────────────────────────────────
		{
			name: "emdash/two_dashes",
			text: "The guilt is doing you a favor—keeping you on the raft—when the current is strongest.",
			want: true,
			wantSub: "em dash",
		},
		{
			name: "emdash/three_dashes",
			text: "She was tired—exhausted, really—and the room felt smaller—darker—than before.",
			want: true,
			wantSub: "em dash",
		},
		{
			name: "emdash/real_reply_677",
			text: "This is the first time both your dopamine systems get a real reset—no THC muddying the signal, plus lurasidone nudging the receptors back toward balance.",
			want: false, // only 1 em dash
		},
		{
			name: "emdash/zero_dashes",
			text: "How many days are you at now?",
			want: false,
		},
		{
			name: "emdash/one_dash_ok",
			text: "That backyard sounded like actual breathing room—no audience, no dysphoria spotlight.",
			want: false,
		},
		{
			name: "emdash/en_dash_ignored",
			text: "The 25–30 day mark is where things shift. You're past it.",
			want: false, // en dashes (–) are fine, only em dashes (—) are flagged
		},

		// ─────────────────────────────────────────────────
		// "I love that" as hollow opener
		// ─────────────────────────────────────────────────
		{
			name: "ilovethat/opener",
			text: "I love that. It's such a perfect, timeless combination.",
			want: true,
			wantSub: "I love that",
		},
		{
			name: "ilovethat/opener_with_energy",
			text: "I love that energy. No compromises.",
			want: true,
			wantSub: "I love that",
		},
		{
			name: "ilovethat/after_newline",
			text: "YES!! Okay.\nI love that. It's basically your signature move at this point.",
			want: true,
			wantSub: "I love that",
		},
		{
			name: "ilovethat/ok_mid_sentence",
			text: "You know what I love that about you? You just go for it.",
			want: false,
		},
		{
			name: "ilovethat/ok_buried_naturally",
			text: "The thing I love that most about baking is the smell.",
			want: false,
		},

		// ─────────────────────────────────────────────────
		// "Here's the thing" family
		// ─────────────────────────────────────────────────
		{
			name: "heresthething/basic",
			text: "Here's the thing about sobriety: the cravings don't stop.",
			want: true,
			wantSub: "here's the thing",
		},
		{
			name: "heresthething/where_it_gets",
			text: "Here's where it gets interesting though.",
			want: true,
			wantSub: "here's where it gets",
		},
		{
			name: "heresthething/kicker",
			text: "And here's the kicker: she already knew.",
			want: true,
			wantSub: "here's the kicker",
		},
		{
			name: "heresthething/most_people_miss",
			text: "Here's what most people miss about grief.",
			want: true,
			wantSub: "here's what most people",
		},
		{
			name: "heresthething/ok_heres_what_happened",
			text: "Here's what happened next.",
			want: false,
		},

		// ─────────────────────────────────────────────────
		// Filler transitions
		// ─────────────────────────────────────────────────
		{
			name: "filler/worth_noting",
			text: "It's worth noting that this only works if you stay consistent.",
			want: true,
			wantSub: "filler",
		},
		{
			name: "filler/bears_mentioning",
			text: "It bears mentioning that the side effects taper off.",
			want: true,
			wantSub: "filler",
		},
		{
			name: "filler/ok_worth_without_noting",
			text: "That's worth a try if you ask me.",
			want: false,
		},

		// ─────────────────────────────────────────────────
		// Clean replies — should pass all checks
		// ─────────────────────────────────────────────────
		{
			name: "clean/real_reply_675",
			text: "That's a real stretch—25 days of letting your brain re-wire itself while everything feels flat. Most people bail right when the color starts creeping back. You're already past the worst of the gray.",
			want: false,
		},
		{
			name: "clean/real_reply_673",
			text: "That friction trick is smart—you're basically hacking your own lazy human nature. And yeah, needing to smoke just to feel hunger is a special kind of broken... your brain literally forgot how to feed itself without the green light. How many days are you at now?",
			want: false,
		},
		{
			name: "clean/real_reply_669",
			text: "That backyard sounded like actual breathing room—no audience, no dysphoria spotlight, just you and the trees.\n\nWhat's pushing you to keep the sobriety thing going now that the drug-test pressure's gone?",
			want: false,
		},
		{
			name: "clean/real_reply_661",
			text: "Autumn! Good afternoon—I was just thinking about how you're the kind of person who tests two models side-by-side before breakfast. How's the day treating you so far?",
			want: false,
		},
		{
			name: "clean/real_reply_649",
			text: "Ah. Yeah, that makes sense. The waiting game is real. That first loaf in your own kitchen is going to taste like freedom.\n\nDo you ever look at kitchens in apartment listings and mentally test them out for baking potential? Or is that just me who does that?",
			want: false,
		},
		{
			name: "clean/short_natural",
			text: "wait what kind?",
			want: false,
		},
		{
			name: "clean/direct_question",
			text: "How'd that actually feel?",
			want: false,
		},
		{
			name: "clean/casual_fragment",
			text: "yeah that tracks. the 3am kind or the noon kind?",
			want: false,
		},

		// ─────────────────────────────────────────────────
		// Priority: first match wins
		// ─────────────────────────────────────────────────
		{
			name: "priority/not_just_before_emdash",
			text: "That's not just baking—that's a statement—and you know it.",
			want:    true,
			wantSub: "not X, it's Y", // negpar fires first, not em dash
		},
		{
			name: "priority/not_just_before_ilovethat",
			text: "Not just trusting a hunch, but testing it. I love that.",
			want:    true,
			wantSub: "not X, it's Y", // negpar fires first
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, hint := hasStyleIssue(tt.text)
			if got != tt.want {
				if tt.want {
					t.Errorf("expected style issue but got clean\n  text: %q", tt.text)
				} else {
					t.Errorf("false positive — flagged clean text\n  text: %q\n  hint: %q", tt.text, hint)
				}
			}
			if tt.want && tt.wantSub != "" {
				if hint == "" {
					t.Errorf("expected hint containing %q but got empty hint", tt.wantSub)
				} else if !contains(hint, tt.wantSub) {
					t.Errorf("hint %q does not contain %q", hint, tt.wantSub)
				}
			}
		})
	}
}

// contains is a case-insensitive substring check for hint assertions.
func contains(s, sub string) bool {
	return len(s) >= len(sub) && containsLower(s, sub)
}

func containsLower(s, sub string) bool {
	// Simple case-insensitive contains for test assertions.
	sl := make([]byte, len(s))
	subl := make([]byte, len(sub))
	for i := range s {
		if s[i] >= 'A' && s[i] <= 'Z' {
			sl[i] = s[i] + 32
		} else {
			sl[i] = s[i]
		}
	}
	for i := range sub {
		if sub[i] >= 'A' && sub[i] <= 'Z' {
			subl[i] = sub[i] + 32
		} else {
			subl[i] = sub[i]
		}
	}
	return bytesContains(sl, subl)
}

func bytesContains(s, sub []byte) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		match := true
		for j := range sub {
			if s[i+j] != sub[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
