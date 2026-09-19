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

PROMPT = """Minimalist geometric logo mark for a developer tool called "Switcher".

The product: a tiny local tool that switches which paid AI subscription
account is active. One account in, another account out.

The mark: exactly two horizontal rounded bars, one above the other. The top
bar carries a triangular arrowhead pointing right. The bottom bar carries a
triangular arrowhead pointing left. They suggest two accounts trading
places. That is the entire composition.

Style requirements:
- deep green #2f9e5f glyph on a fully transparent background
- flat vector, single color, no gradients, no shadows, no glow, no texture
- uniform stroke thickness like a well-known line icon set (feather style)
- generous negative space, optically centered, strict 24 by 24 grid logic
- crisp rounded geometry that stays readable at 16 pixels and at 512 pixels
- no text, no letters, no wordmark, no border, no background tile, no
  rounded-square container, no circle

Forbidden: avatars, person silhouettes, heads, faces, user icons, brains,
robots, sparkles, lightning bolts, arrows arranged in a circle, refresh
icons, shields, locks, keyholes, existing company logos, gradients,
multiple colors."""


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--model", default="gpt-image-2")
    parser.add_argument("--out", default="web")
    parser.add_argument("--count", type=int, default=3,
                        help="how many candidates to render (they are saved "
                             "as logo-1.png, logo-2.png, ...)")
    args = parser.parse_args()

    key = os.environ.get("OPENAI_API_KEY")
    if not key:
        sys.exit("error: set OPENAI_API_KEY in the environment first")

    out_dir = os.path.abspath(args.out)
    os.makedirs(out_dir, exist_ok=True)

    # One candidate per request: identical prompts drift anyway, and three
    # attempts beat one gamble when you cannot see the result beforehand.
    for index in range(1, args.count + 1):
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
        print(f"rendering candidate {index}/{args.count} ...")
        with urllib.request.urlopen(req, timeout=180) as resp:
            payload = json.load(resp)

        item = payload["data"][0]
        if "b64_json" in item and item["b64_json"]:
            png = base64.b64decode(item["b64_json"])
        elif "url" in item and item["url"]:
            with urllib.request.urlopen(item["url"], timeout=120) as img:
                png = img.read()
        else:
            sys.exit(f"unexpected response shape: {list(item)}")

        target = os.path.join(out_dir, f"logo-{index}.png")
        with open(target, "wb") as f:
            f.write(png)
        print(f"wrote {target} ({len(png)} bytes)")

    print("pick the one you like, copy it over web/logo.png, and refresh")


if __name__ == "__main__":
    main()
