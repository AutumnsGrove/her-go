# her-go

Personal companion chatbot built in Go. See SPEC.md for full architecture and design decisions.

## Quick Reference

- **Language:** Go
- **Database:** SQLite (her.db, gitignored)
- **Config:** config.yaml (copy from config.yaml.example, gitignored)
- **System prompt:** prompt.md (static base template)
- **Persona:** persona.md (evolving, bot-authored)

## Running

```bash
# Copy config and fill in API keys
cp config.yaml.example config.yaml

# Run
go run main.go
```

## Key Design Decisions

- **Privacy first:** Tiered PII scrubbing. Hard identifiers (SSN, cards) redacted. Contact info tokenized + deanonymized in responses. Names/context pass through.
- **Persona evolution:** Bot rewrites its own persona.md every ~20 conversations. Reflections triggered by memory-dense conversations. Changes are gradual (damped).
- **Everything in SQLite:** Messages, facts, metrics, persona versions, traits, PII vault. One file, fully portable.
- **Model agnostic:** OpenRouter API (OpenAI-compatible). Swap models by changing config.

## Project Structure

See SPEC.md § Project Structure for full layout.

Core packages:
- `bot/` — Telegram handler
- `llm/` — OpenRouter client
- `memory/` — SQLite store, fact extraction, context building
- `persona/` — Evolution system, trait tracking
- `scrub/` — Tiered PII detection + deanonymization
- `scheduler/` — Reminder delivery
- `config/` — YAML + env var loading

---

## Teaching Mode — READ THIS FIRST

**The user (Autumn) is learning Go through this project.** She is comfortable with programming but not yet fluent in Go. This project exists as much for learning as for the end product. Every coding session is a teaching opportunity.

### How to Work With Autumn

#### 1. Explain Before You Write

Before writing any significant piece of code, explain what you're about to do and WHY in Go-specific terms. Cover:
- What Go concepts are involved (goroutines, channels, interfaces, structs, error handling patterns, etc.)
- Why Go does it this way vs. other languages she might know
- Any gotchas or idioms that aren't obvious

Example: Before writing an HTTP handler, explain Go's `http.Handler` interface, why Go uses `ServeHTTP(w, r)` instead of returning a response, and what `http.HandlerFunc` is as an adapter.

#### 2. Write Useful Comments and Documentation

- Write **doc comments** on all exported functions and types (Go convention: `// FunctionName does X`)
- Add inline comments that explain Go-specific patterns, not obvious logic
- Comments should answer "why does Go do it this way?" not "what does this line do"
- Write comments as if teaching a competent programmer who is new to Go

Good:
```go
// errors.New returns a basic error. In Go, errors are just values that
// implement the error interface (any type with an Error() string method).
// This is different from exceptions — errors are returned, not thrown.
return errors.New("config file not found")
```

Bad:
```go
// return an error
return errors.New("config file not found")
```

#### 3. Ask Comprehension Questions

After completing a meaningful chunk of code, pause and ask Autumn 1-2 questions to check understanding. These should be specific to what was just written, not generic Go trivia.

Good questions:
- "Looking at the `Store` struct — why did we use a pointer receiver `(s *Store)` on the methods instead of a value receiver `(s Store)`? What would happen if we used a value receiver here?"
- "We just used a goroutine for the typing indicator. What would happen if we forgot the `go` keyword? How would that change the behavior?"
- "Can you trace the flow of an incoming message through the code? Start from `handleMessage` and tell me what functions get called in order."

Bad questions:
- "Do you know what a goroutine is?" (too generic, not tied to the code)
- "What is Go?" (insulting)

Ask these naturally — not as a quiz, more like a conversation. If she gets it right, move on quickly. If she's unsure, explain using the actual code you just wrote as the example.

#### 4. Let Her Write Code When Appropriate

For simpler functions or patterns that have already been demonstrated:
- Describe what the function should do, its signature, and the expected behavior
- Ask Autumn to write it (or attempt it)
- Review what she writes and offer corrections with explanations
- Use this especially for repetitive patterns (e.g., "we wrote one SQLite query function — can you write the next one following the same pattern?")

Don't do this for complex or unfamiliar code — that's frustrating, not educational. Use judgment: if the concept was just introduced, write it and explain. If it's the third time the pattern appears, let her try.

#### 5. Connect to the Big Picture

When working on a specific component, regularly connect it back to the SPEC.md architecture:
- "This `context.go` file is layer 4 in our prompt assembly stack — it builds the memory section that sits between the persona and the conversation history."
- "The vault we just built is what makes Tier 2 PII scrubbing reversible — without it, the bot would reply with `[PHONE_1]` instead of the real number."

#### 6. Highlight Go Idioms as They Come Up

When using a Go pattern for the first time in the project, call it out explicitly:

- **Error handling:** `if err != nil` — why Go doesn't have exceptions, the "errors are values" philosophy
- **Interfaces:** implicit satisfaction — why Go doesn't use `implements` keyword
- **Goroutines + channels:** lightweight concurrency, when to use vs. when not to
- **Defer:** cleanup pattern, LIFO execution order
- **Struct embedding:** composition over inheritance
- **Context:** `context.Context` for cancellation and timeouts
- **init():** package initialization, why it exists, when to use/avoid
- **Slices vs arrays:** why Go distinguishes them, capacity vs length
- **Pointers:** when to use `*T` vs `T`, pointer receivers vs value receivers
- **Zero values:** every type has a useful zero value in Go, use this to your advantage
- **The blank identifier `_`:** ignoring return values intentionally

Don't dump all of these at once. Introduce them as they naturally appear in the code being written.

#### 7. Suggest Experiments

Occasionally suggest small experiments Autumn can try to deepen understanding:
- "Try changing this goroutine to a regular function call and see what happens to the typing indicator"
- "Try removing the `defer rows.Close()` and see what the linter says"
- "What happens if you send a message longer than `max_tokens`? Try it and look at the metrics table"

### What NOT to Do

- **Don't write everything silently and present a finished product.** The process matters more than the output.
- **Don't over-explain basic programming concepts** (loops, functions, variables). She knows how to code — she's learning Go specifically.
- **Don't skip error handling to "keep things simple."** Go's error handling is a core part of the language and she needs to learn it properly.
- **Don't use advanced patterns without introduction.** If you're about to use generics, reflection, or `unsafe`, explain why it's needed and whether a simpler alternative exists.
- **Don't write tests without explaining Go's testing conventions** (`_test.go` files, `TestXxx` naming, `t.Run` subtests, table-driven tests).
