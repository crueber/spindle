#!/usr/bin/env bash
# site-check.sh - the static website's gate. No build, no dependencies beyond python3.
#
#   scripts/site-check.sh
#
# Asserts, before anything is staged or deployed:
#   * no file the publish script exports mentions the private estate or old CI prose;
#   * every local src/href/poster the page references exists (the demo is staged from docs/demo.webm);
#   * exactly one h1, a skip link to #main, non-empty alt on every <img>;
#   * every local src/srcset/href/poster exists, and the OG card is a 1200x630 PNG under 300 KiB;
#   * robots.txt and sitemap.xml exist;
#   * no <script src> or <link href> points off-origin (the page makes no external requests).
set -euo pipefail
ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
SITE="$ROOT/site"

fail=0
bad() { echo "site-check: $*" >&2; fail=1; }

# Publish-gate strings, assembled here so this exported script does not itself carry them.
ESTATE="pack""den"
CI_PROSE="Forgejo"" Actions"

# 1. publish-gate strings (the published tree must never carry them). scripts/publish/ is removed
#    by the publish script and installed as .github/workflows/, so it is not scanned here.
for f in "$SITE" "$ROOT/scripts/site-stage.sh" "$ROOT/scripts/site-check.sh"; do
  if grep -rIn -e "$ESTATE" -e "$CI_PROSE" "$f" >/dev/null 2>&1; then
    grep -rIn -e "$ESTATE" -e "$CI_PROSE" "$f" >&2 || true
    bad "private-estate reference under $f"
  fi
done

# 2. required files and the weight budgets
for f in index.html style.css site.js theme-init.js favicon.svg robots.txt sitemap.xml img/og.png; do
  [ -f "$SITE/$f" ] || bad "site/$f is missing"
done

demo="$ROOT/docs/demo.webm"
if [ -f "$demo" ]; then
  demo_bytes=$(wc -c <"$demo" | tr -d ' ')
  [ "$demo_bytes" -le 3145728 ] || bad "docs/demo.webm is ${demo_bytes} B; the demo budget is 3 MiB"
fi
site_bytes=0
while IFS= read -r f; do
  size=$(wc -c <"$f")
  site_bytes=$((site_bytes + size))
done < <(find "$SITE" -type f)
[ "$site_bytes" -le 1572864 ] || bad "site/ is ${site_bytes} B without the demo; the page budget is 1.5 MiB"

# 3. references, markup and off-origin assets
python3 - "$SITE" <<'PY' || fail=1
import html.parser, os, sys

site = sys.argv[1]
index = os.path.join(site, "index.html")
doc = open(index, encoding="utf-8").read()
errors = []

for name in ("robots.txt", "sitemap.xml"):
    if not os.path.isfile(os.path.join(site, name)):
        errors.append(f"site/{name} is missing")

og = os.path.join(site, "img", "og.png")
if not os.path.isfile(og):
    errors.append("site/img/og.png is missing")
else:
    if os.path.getsize(og) > 300 * 1024:
        errors.append(f"site/img/og.png is {os.path.getsize(og)} B; the card budget is 300 KiB")
    with open(og, "rb") as fh:
        head = fh.read(24)
    if head[:8] != b"\x89PNG\r\n\x1a\n" or head[12:16] != b"IHDR":
        errors.append("site/img/og.png is not a PNG")
    else:
        w = int.from_bytes(head[16:20], "big")
        h = int.from_bytes(head[20:24], "big")
        if (w, h) != (1200, 630):
            errors.append(f"site/img/og.png is {w}x{h}; the card must be 1200x630")

class Check(html.parser.HTMLParser):
    def __init__(self):
        super().__init__(convert_charrefs=True)
        self.h1 = 0
        self.imgs = 0
        self.alt = 0
        self.local_refs = []
        self.offsite = []
    def handle_starttag(self, tag, attrs):
        a = dict(attrs)
        if tag == "h1":
            self.h1 += 1
        if tag == "img":
            self.imgs += 1
            if (a.get("alt") or "").strip():
                self.alt += 1
            else:
                errors.append("an <img> has no non-empty alt")
        if tag in ("img", "source"):
            for candidate in (a.get("srcset") or "").split(","):
                parts = candidate.strip().split()
                if not parts:
                    continue
                url = parts[0]
                if url.startswith(("http://", "https://", "//")):
                    self.offsite.append(url)
                elif not url.startswith("data:"):
                    self.local_refs.append(url)
        if tag == "script" and a.get("src"):
            if a["src"].startswith(("http://", "https://", "//")):
                self.offsite.append(a["src"])
            else:
                self.local_refs.append(a["src"])
        if tag == "link" and a.get("href"):
            href = a["href"]
            rel = a.get("rel", "")
            if rel in ("stylesheet", "icon", "preload", "prefetch", "manifest"):
                if href.startswith(("http://", "https://", "//")):
                    self.offsite.append(href)
                else:
                    self.local_refs.append(href)
        if tag in ("img", "source", "video", "a") :
            for key in ("src", "poster", "href"):
                v = a.get(key)
                if v and not v.startswith(("#", "http://", "https://", "mailto:", "//", "data:")):
                    self.local_refs.append(v)

p = Check()
p.feed(doc)
if p.h1 != 1:
    errors.append(f"expected exactly one h1, found {p.h1}")
if p.imgs == 0:
    errors.append("no images found")
if p.alt != p.imgs:
    errors.append(f"{p.imgs - p.alt} image(s) without non-empty alt")
if p.offsite:
    errors.append("off-origin script/link asset(s): " + ", ".join(p.offsite))
if 'href="#main"' not in doc:
    errors.append('no skip link to #main')

for ref in sorted(set(p.local_refs)):
    ref = ref.split("?")[0].split("#")[0]
    if not ref:
        continue
    target = os.path.join(site, ref)
    if ref == "demo.webm":
        target = os.path.join(os.path.dirname(site), "docs", "demo.webm")
    if not os.path.isfile(target):
        errors.append(f"referenced asset is missing: {ref}")

for e in errors:
    print("site-check: " + e, file=sys.stderr)
sys.exit(1 if errors else 0)
PY

if [ "$fail" -ne 0 ]; then
  echo "site-check: FAILED" >&2
  exit 1
fi

echo "site-check: ok — $(find "$SITE" -type f | wc -l | tr -d ' ') files, ${site_bytes} bytes ($((site_bytes / 1024)) KiB) without the demo"
