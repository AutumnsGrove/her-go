---
name: web_read
description: "Read and extract clean text content from a specific URL using the Tavily Extract API"
version: "1.0.0"
language: go
hash: "sha256:01ca0bb74cc11a4d0598ccaae01f77ab16589f9a5b8764fff2e3d552c50a5e88"
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
