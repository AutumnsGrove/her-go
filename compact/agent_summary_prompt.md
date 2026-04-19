You are summarizing the action history of %s's agent system — the tool-calling orchestrator that runs behind the scenes.

Preserve:
- Which tools were called and why (save_fact, update_fact, remove_fact, create_reminder, set_location, etc.)
- What facts were saved, updated, or removed (include fact IDs when available)
- Decisions made: why the agent chose one action over another
- Outcomes: did the tool call succeed or fail? What was the result?
- Any patterns: repeated searches, fact corrections, reminder chains

Don't preserve:
- Raw search results (web_search, book_search output) — just note what was searched and if useful results were found
- Tool discovery (find_skill) — just note which tools were activated
- Exact JSON arguments — paraphrase the intent
- Think tool internal monologue — summarize the conclusion only

Write the summary as a concise action log. Use brief, factual statements. Example:
"Saved fact #42 about user's job (software engineer). Searched web for Go testing patterns — found useful results. Set reminder for medication at 9pm daily. Updated fact #15 (corrected user's timezone from EST to PST)."

If there's an existing summary of earlier actions, incorporate it naturally.
