// Package llm's Nova integration lives here.
//
// This file exists in the "Go bump" commit for one reason: to keep
// github.com/AutumnsGrove/nova pinned in go.mod through `go mod tidy`. Go
// prunes a module from the build list if nothing imports it, and the tidy in
// the toolchain-bump commit also drags in the charm.land minor bumps — where
// that churn lands should be its own reviewable commit, so the dependency is
// imported (blankly) here and the real code replaces this import in the
// provider-seam commit (LLM facade over Nova's provider).
//
// The blank identifier `_` is Go's "import for the side effect only" idiom:
// the package's init() runs — for Nova's provider packages there is nothing to
// do, so the effect is really just "stay in the module graph" — while no names
// become available. It reads oddly out of context, which is why it gets a
// comment. See docs/plans/PLAN-nova-migration.md, step 1.3.
package llm

import (
	_ "github.com/AutumnsGrove/nova/provider/openrouter"
)
