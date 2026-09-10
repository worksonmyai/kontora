#!/usr/bin/env python3
"""Normalize an OpenRouter Models API response into the bundled price catalog.

Examples:
    update-model-prices.py response.json internal/pricing/catalog.json
    update-model-prices.py --url https://openrouter.ai/api/v1/models --output catalog.json
    OPENROUTER_MODELS_URL=https://example.test/models update-model-prices.py

The default output is internal/pricing/catalog.json. When no input file is
provided, the URL comes from OPENROUTER_MODELS_URL, falling back to the public
OpenRouter Models API.
"""

from __future__ import annotations

import argparse
import json
import os
from pathlib import Path
import re
import stat
import sys
import tempfile
from typing import Any
from urllib.request import Request, urlopen

DEFAULT_URL = "https://openrouter.ai/api/v1/models"
DEFAULT_OUTPUT = Path("internal/pricing/catalog.json")
RATE_NAMES = ("prompt", "completion", "input_cache_read", "input_cache_write")
DECIMAL = re.compile(r"^-?(?:0|[1-9][0-9]*)(?:\.[0-9]+)?$")
SOURCE = {
    "name": "OpenRouter",
    "url": DEFAULT_URL,
    "documentation_url": "https://openrouter.ai/docs/guides/overview/models",
    "attribution": "Model pricing data provided by OpenRouter (https://openrouter.ai/).",
    "rate_basis": "Standard top-provider text token prices in USD per token",
}


class CatalogError(ValueError):
    pass


def parse_args(argv: list[str]) -> tuple[str | None, str, Path]:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "input",
        nargs="?",
        help="Models API JSON file (omit to fetch the configured URL)",
    )
    parser.add_argument(
        "positional_output",
        nargs="?",
        metavar="OUTPUT",
        help="catalog path (default: internal/pricing/catalog.json)",
    )
    parser.add_argument(
        "--url",
        help="Models API URL; defaults to $OPENROUTER_MODELS_URL or OpenRouter",
    )
    parser.add_argument("-o", "--output", type=Path, help="catalog path")
    args = parser.parse_args(argv)

    if args.input and args.url:
        parser.error("INPUT and --url are mutually exclusive")
    if args.positional_output and args.output:
        parser.error("positional OUTPUT and --output are mutually exclusive")

    output = args.output or (
        Path(args.positional_output) if args.positional_output else DEFAULT_OUTPUT
    )
    url = args.url or os.environ.get("OPENROUTER_MODELS_URL", DEFAULT_URL)
    return args.input, url, output


def read_response(input_path: str | None, url: str) -> Any:
    if input_path is not None:
        with open(input_path, "r", encoding="utf-8") as source:
            return json.load(source)

    request = Request(url, headers={"User-Agent": "kontora-model-price-refresh/1"})
    with urlopen(request, timeout=60) as response:  # noqa: S310 - maintainer-selected URL
        return json.load(response)


def normalize(response: Any) -> dict[str, Any]:
    if not isinstance(response, dict) or not isinstance(response.get("data"), list):
        raise CatalogError("response root must contain a data array")

    if not response["data"]:
        raise CatalogError("response data array must not be empty")

    seen: set[str] = set()
    models: list[dict[str, Any]] = []
    for index, source_model in enumerate(response["data"]):
        if not isinstance(source_model, dict):
            raise CatalogError(f"data[{index}] must be an object")

        model_id = source_model.get("id")
        if not isinstance(model_id, str) or not model_id.strip():
            raise CatalogError(f"data[{index}].id must be a non-empty string")
        if model_id in seen:
            raise CatalogError(f"duplicate model id: {model_id}")
        seen.add(model_id)

        source_pricing = source_model.get("pricing")
        if not isinstance(source_pricing, dict):
            raise CatalogError(f"model {model_id!r} must contain a pricing object")

        pricing: dict[str, str] = {}
        for name in RATE_NAMES:
            if name not in source_pricing:
                continue
            value = source_pricing[name]
            if not isinstance(value, str) or DECIMAL.fullmatch(value) is None:
                raise CatalogError(
                    f"model {model_id!r} pricing.{name} must be a decimal string"
                )
            # Preserve the API string as written. Trailing zeros record the
            # source precisely, and -1 denotes a dynamic router.
            pricing[name] = value

        models.append({"id": model_id, "pricing": pricing})

    models.sort(key=lambda model: model["id"])
    return {"source": SOURCE, "models": models}


def atomic_write(path: Path, catalog: dict[str, Any]) -> None:
    path = path.resolve()
    path.parent.mkdir(parents=True, exist_ok=True)
    mode = 0o644
    try:
        mode = stat.S_IMODE(path.stat().st_mode)
    except FileNotFoundError:
        pass

    fd, temporary_name = tempfile.mkstemp(
        dir=path.parent, prefix=f".{path.name}.", suffix=".tmp"
    )
    temporary = Path(temporary_name)
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as output:
            json.dump(catalog, output, indent=2, ensure_ascii=False)
            output.write("\n")
            output.flush()
            os.fchmod(output.fileno(), mode)
            os.fsync(output.fileno())
        os.replace(temporary, path)
    finally:
        # os.replace removes the temporary name on success. On every failure,
        # remove it without touching the previous destination.
        try:
            temporary.unlink()
        except FileNotFoundError:
            pass


def main(argv: list[str]) -> int:
    input_path, url, output = parse_args(argv)
    try:
        response = read_response(input_path, url)
        catalog = normalize(response)
        atomic_write(output, catalog)
    except (CatalogError, json.JSONDecodeError, OSError, ValueError) as error:
        print(f"update-model-prices: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
