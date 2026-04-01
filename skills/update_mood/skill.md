---
name: update_mood
description: "Update the most recent mood entry when the user's emotional state has shifted"
version: "1.0.0"
language: go
hash: "sha256:8eba43b99ef8dad960490e1568c5780f0ad762f33325e31f36d54ec95231bc7f"
author: autumn
params:
  - name: rating
    type: int
    required: true
    description: "New mood rating: 1=bad, 2=rough, 3=meh/neutral, 4=good, 5=great"
  - name: note
    type: string
    required: true
    description: "Brief context for the updated rating"
permissions:
  db:
    - mood_entries:rw
  timeout: 5s
---

## Instructions

Update the most recent mood entry when the user's mood has shifted but a new entry was recently logged. This overwrites the previous rating and note on the latest entry, keeping the chart clean while still tracking genuine emotional changes.

Use this instead of log_mood when:
- A mood was already logged in the last 30 minutes but the user's emotional state has clearly changed
- The conversation reveals the previous mood assessment was wrong or incomplete

Do NOT use this to make minor wording tweaks — only when the rating or emotional tone has meaningfully shifted.
