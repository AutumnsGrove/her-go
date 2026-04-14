---
name: web_search
description: "Search the web for current information, recent events, and factual answers using the Tavily API"
version: "1.0.0"
language: go
hash: "sha256:f6e288d1a1b78a60a734e78fc7e2c4297a1dbc4395c7c0bf765b9bd87c7be8e4"
author: autumn
params:
  - name: query
    type: string
    required: true
    description: "The search query"
  - name: limit
    type: int
    required: false
    default: 5
    description: "Maximum number of results to return"
permissions:
  network: true
  domains:
    - api.tavily.com
  env:
    - TAVILY_API_KEY
  timeout: 15s
requires:
  env: [TAVILY_API_KEY]
---

## Instructions

Search the web for current, factual information. Use when the user asks about:
- Recent events or news
- Questions beyond your training data
- Anything that benefits from real-time information
- Fact-checking or verification

The skill returns a summary answer (when available) plus ranked source results
with titles, URLs, and content snippets.
