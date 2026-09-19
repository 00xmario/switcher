#!/usr/bin/env python3
"""Generate the Switcher logo with OpenAI image generation.

Reads OPENAI_API_KEY from the environment, renders the logo prompt below,
and writes a transparent PNG to web/logo.png (plus a web/logo-full.png
preview at full resolution).

Usage:
    export OPENAI_API_KEY=sk-...
    python3 scripts/generate-logo.py [--model gpt-image-2] [--out web]
"""

import argparse
import base64
import json
import os
import sys
import urllib.request

API_URL = "https://api.openai.com/v1/images/generations"

PROMPT = """Flat vector app icon for a developer tool called "Switcher".

The product: a tiny local tool that switches AI coding subscriptions between
multiple paid accounts; exactly one account is active at a time.

Logo concept: two stacked rounded-rectangle "account cards", the back one
translucent and offset, the front one solid, with a clean double-headed swap
arrow beside them pointing both up and down. The composition reads instantly
as "switch between accounts".

Style requirements:
- glyph color: deep brand green #2f9e5f on a fully transparent background
- flat vector, no gradients, no shadows, no glow, no texture
- geometric precision: consistent stroke weight, generous negative space,
  optically centered, aligned to a strict grid
- rounded-corner geometry matching modern macOS app icon language
- no text, no letters, no wordmark, no border, no rounded-square container
- single-color glyph, crisp edges, works at 16px and at 512px

Avoid: generic lightning bolts, arrows in a circle, refresh icons, default
startup gradients, sparkles, brain imagery, robot imagery, any existing
company's logo."""


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--model", default="gpt-image-2")
    parser.add_argument("--out", default="web")
    args = parser.parse_args()

    key = os.environ.get("OPENAI_API_KEY")
    if not key:
        sys.exit("error: set OPENAI_API_KEY in the environment first")

    body = json.dumps({
        "model": args.model,
        "prompt": PROMPT,
        "size": "1024x1024",
        "background": "transparent",
        "output_format": "png",
    }).encode()

    req = urllib.request.Request(
        API_URL,
        data=body,
        headers={
            "Authorization": f"Bearer {key}",
            "Content-Type": "application/json",
        },
        method="POST",
    )
    with urllib.request.urlopen(req, timeout=180) as resp:
        payload = json.load(resp)

    item = payload["data"][0]
    out_dir = os.path.abspath(args.out)
    os.makedirs(out_dir, exist_ok=True)

    if "b64_json" in item and item["b64_json"]:
        png = base64.b64decode(item["b64_json"])
    elif "url" in item and item["url"]:
        with urllib.request.urlopen(item["url"], timeout=120) as img:
            png = img.read()
    else:
        sys.exit(f"unexpected response shape: {list(item)}")

    full = os.path.join(out_dir, "logo-full.png")
    with open(full, "wb") as f:
        f.write(png)
    print(f"wrote {full} ({len(png)} bytes)")

    # The UI ships the full-resolution PNG directly; it is a single icon and
    # PNG compression keeps it small. Downscale copies can be added later.
    target = os.path.join(out_dir, "logo.png")
    if os.path.exists(full) and not os.path.exists(target):
        os.replace(full, target)
        print(f"wrote {target}")
    print("done")


if __name__ == "__main__":
    main()
