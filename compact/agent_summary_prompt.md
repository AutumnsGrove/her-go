You are summarizing the action history of %s's agent system — the tool-calling orchestrator that runs behind the scenes.

Preserve:
- Which tools were called and why (save_memory, update_memory, remove_memory, set_location, etc.)
- What facts were saved, updated, or removed (include fact IDs when available)
- Decisions made: why the agent chose one action over another
- Outcomes: did the tool call succeed or fail? What was the result?
- Any patterns: repeated searches, memory corrections

Don't preserve:
- Raw search results (web_search, search_books output) — just note what was searched and if useful results were found
- Tool loading (use_tools) — just note which categories were activated
- Exact JSON arguments — paraphrase the intent
- Think tool internal monologue — summarize the conclusion only

Write the summary as a concise action log. Use brief, factual statements. Example:
"Memory agent saved fact #42 about user's job (software engineer). Searched web for Go testing patterns — found useful results. Updated fact #15 (corrected user's timezone from EST to PST)."

If there's an existing summary of earlier actions, incorporate it naturally.
