#!/usr/bin/env python3
"""Regenerate the self-hosted web fonts from a Google Fonts css2 response.

Usage: build_fonts.py <raw_css> <out_dir>

Reads the css2 output at <raw_css>, keeps the latin and latin-ext @font-face
blocks (enough for an English UI), downloads each woff2 into <out_dir>, and
writes <out_dir>/fonts.css with the gstatic URLs rewritten to local filenames.
Invoked by hack/vendor-assets.sh.
"""
import re
import sys
import urllib.request

KEEP = {"latin", "latin-ext"}


def main():
    if len(sys.argv) != 3:
        sys.exit("usage: build_fonts.py <raw_css> <out_dir>")
    raw_path, out_dir = sys.argv[1], sys.argv[2]
    raw = open(raw_path, encoding="utf-8").read()

    block_re = re.compile(r"/\*\s*([\w-]+)\s*\*/\s*(@font-face\s*\{[^}]*\})", re.S)
    family_re = re.compile(r"font-family:\s*'([^']+)'")
    url_re = re.compile(r"url\((https://[^)]+\.woff2)\)")

    out = []
    for subset, block in block_re.findall(raw):
        if subset not in KEEP:
            continue
        fam = family_re.search(block).group(1)
        url = url_re.search(block).group(1)
        local = f"{fam.lower().replace(' ', '-')}-{subset}.woff2"
        urllib.request.urlretrieve(url, f"{out_dir}/{local}")
        out.append(f"/* {fam} {subset} */\n{url_re.sub(f'url({local})', block)}\n")
        print(f"  {local}", file=sys.stderr)

    open(f"{out_dir}/fonts.css", "w", encoding="utf-8").write("\n".join(out))


if __name__ == "__main__":
    main()
