---
name: log_mood
description: "Log the user's current emotional state with a 1-5 rating and optional note"
version: "1.0.0"
language: go
hash: "sha256:885cf8b430d60f62c90d3202e6f3acd3429cbbd597f8173ab46f1d3735da8c5d"
author: autumn
params:
  - name: rating
    type: int
    required: true
    description: "Mood rating: 1=bad, 2=rough, 3=meh/neutral, 4=good, 5=great"
  - name: note
    type: string
    required: true
    description: "Brief context for the rating"
permissions:
  db:
    - mood_entries:rw
  timeout: 5s
---

## Instructions

Log the user's mood when they express how they're feeling. Use a 1-5 scale:
1 = bad, 2 = rough, 3 = meh/neutral, 4 = good, 5 = great.

Always include a brief note capturing the reason or context if mentioned.
If the user just says "I'm good" with no context, use a short note like "feeling good".
