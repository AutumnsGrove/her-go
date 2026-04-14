---
name: book_search
description: "Search for book information using the Open Library API — titles, authors, publication dates, subjects, and cover images"
version: "1.0.0"
language: go
hash: "sha256:cabfd05653753583564202a9c5e29a50d456d2fe1ef29af8de3b5ca0973da909"
author: autumn
params:
  - name: query
    type: string
    required: true
    description: "Book title, author name, or search terms"
  - name: limit
    type: int
    required: false
    default: 3
    description: "Maximum number of results to return"
permissions:
  network: true
  domains:
    - openlibrary.org
    - covers.openlibrary.org
  timeout: 10s
requires:
  env: []
  bins: []
---

## Instructions

Search for books using the Open Library API. Use when discussing books,
looking for recommendations, or when the user mentions a book title or author.
No API key needed — Open Library is free and open.

Returns book titles, authors, publication years, page counts, subjects,
and cover image URLs.
