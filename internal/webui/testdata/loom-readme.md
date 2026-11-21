<div align="center">

# Loom

[Quick Start](#quick-start) • [Features](#features)

MIT License — free as in freedom
</div>

---

[![Loom demo: a dark board, markdown notes, link cards — click for the video](docs/demo.webp)](docs/demo.webm)

*Dark → light: notes, links, new cards, drag & drop.*

---

### Features

- **Boards, columns, cards** — switch boards from the navbar; collapse
  columns into slim rails when you need focus.

<details>
<summary><strong>🐳 Docker (recommended)</strong></summary>
<br>

```sh
docker build -t loom .
docker run -p 8080:8080 -v loom-data:/data loom
```

Open [http://localhost:8080](http://localhost:8080). Your database lives
in the `loom-data` volume.

<hr>
</details>

<details>
<summary><strong>🏃 Go run (local dev)</strong></summary>
<br>

```sh
go run ./cmd/loom --addr :8080 --data loom.db --web web
```

On first run with an empty store, Loom seeds a `Home` board.

<hr>
</details>

See [the guide](docs/guide.md) and [the source](src/main.go#L10).
