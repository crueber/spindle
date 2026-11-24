# Boards

<!-- case: a -->

A self-hosted, minimal personal boards app for links and notes. Boards
hold columns, columns hold cards, and each card holds an ordered mix of
links, markdown notes, todo checklists, and images. One small Go binary,
no framework JavaScript, your data in a single SQLite file.

<!-- case: b -->

A hard break needs two trailing spaces  
and this sentence starts on the next line.

<!-- case: c -->

A hard break also takes a trailing backslash\
and this sentence starts on the next line.

<!-- case: d -->

- **Boards** hold columns — switch boards from the navbar; collapse
columns into slim rails when you need focus.
- A second card type is a markdown note whose text
wraps onto an unindented line and stays in the item.

<!-- case: e -->

> A blockquote that wraps at the author's line width
and continues lazily on an unindented line.

<!-- case: f -->

<div align="center">

# Boards

A self-hosted, minimal personal boards app for links and notes<br>
No account, no cloud, no telemetry

</div>

<!-- case: g -->

```sh
docker run -p 8080:8080 \
  -v boards-data:/data \
  boards
```

<!-- case: h -->

| Card type | Stored as |
| --- | --- |
| Link | URL + title |
| Note | Markdown |
