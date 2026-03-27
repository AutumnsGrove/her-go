---
name: web_read
description: "Read and extract clean text content from a specific URL using the Tavily Extract API"
version: "1.0.0"
language: go
author: autumn
params:
  - name: url
    type: string
    required: true
    description: "The URL to read and extract content from"
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

Read a specific URL and extract its text content. Use when the user shares
a link or you need to read a specific web page. Returns the cleaned text
content of the page, truncated to keep prompt sizes reasonable.
