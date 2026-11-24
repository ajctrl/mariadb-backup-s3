"""Discover supported MariaDB LTS series and prepare their first image PR."""

import argparse
from datetime import date, datetime, timezone
import json
from pathlib import Path
import re
import sys
from urllib.request import urlopen


FOUNDATION = "https://downloads.mariadb.org/rest-api/mariadb/"
DOCKER_TAGS = "https://hub.docker.com/v2/namespaces/library/repositories/mariadb/tags"
SERIES = r"[0-9]+\.[0-9]+"
DIGEST = r"sha256:[0-9a-f]{64}"
STAGE = re.compile(rf"^FROM mariadb:({SERIES})\.[0-9]+@{DIGEST} AS client-\1$", re.M)


def fetch(url):
    with urlopen(url, timeout=30) as response:
        return json.load(response)


def supported(dockerfile):
    series = STAGE.findall(dockerfile)
    if not series or len(set(series)) != len(series) or len(series) != dockerfile.count("FROM mariadb:"):
        raise ValueError("Expected one pinned Dockerfile stage per MariaDB series")
    return sorted(series, key=lambda s: tuple(map(int, s.split("."))))


def stable_lts(catalog, today):
    result = set()
    for row in catalog["major_releases"]:
        if row["release_status"] != "Stable" or row["release_support_type"] != "Long Term Support":
            continue
        series = row["release_id"]
        if not re.fullmatch(SERIES, series):
            raise ValueError(f"Unexpected MariaDB series {series!r}")
        if row["release_eol_date"] and date.fromisoformat(row["release_eol_date"]) <= today:
            continue
        result.add(series)
    if not result:
        raise ValueError("MariaDB catalog contained no maintained stable LTS series")
    return result


def published_image(series):
    # Docker Hub can publish the patch tag and the two architectures after GA.
    # Only bare numeric patch tags from the official repository are eligible.
    best = None
    page = 1
    while True:
        tags = fetch(f"{DOCKER_TAGS}?name={series}.&page_size=100&page={page}")
        for tag in tags["results"]:
            match = re.fullmatch(re.escape(series) + r"\.([0-9]+)", tag["name"])
            if not match:
                continue
            platforms = {i["architecture"] for i in tag["images"] if i["os"] == "linux"}
            if not {"amd64", "arm64"} <= platforms or not re.fullmatch(DIGEST, tag.get("digest") or ""):
                continue
            patch = int(match[1])
            if best is None or patch > best[0]:
                best = (patch, tag["name"], tag["digest"])
        if not tags["next"]:
            break
        page += 1
    if best:
        return {"series": series, "version": best[1], "digest": best[2]}
    return None


def discover(existing):
    today = datetime.now(timezone.utc).date()
    candidates = []
    for series in sorted(stable_lts(fetch(FOUNDATION), today) - set(existing)):
        image = published_image(series)
        if image:
            candidates.append(image)
        else:
            print(f"Waiting for both official Linux images for MariaDB {series}", file=sys.stderr)
    return candidates


def add_series(root, series, version, digest):
    if not (re.fullmatch(SERIES, series) and re.fullmatch(re.escape(series) + r"\.[0-9]+", version)
            and re.fullmatch(DIGEST, digest)):
        raise ValueError("Invalid MariaDB image reference")
    dockerfile = root / "Dockerfile"
    source = dockerfile.read_text()
    if series in supported(source):
        return
    last = list(STAGE.finditer(source))[-1].end()
    source = source[:last] + f"\nFROM mariadb:{version}@{digest} AS client-{series}" + source[last:]

    config_path = root / "renovate.json"
    config = json.loads(config_path.read_text())
    config["packageRules"].append({
        "matchDatasources": ["docker"],
        "matchPackageNames": ["mariadb"],
        "matchCurrentVersion": "/^" + series.replace(".", "[.]") + "[.]/",
        "groupName": f"MariaDB {series}",
    })
    readme = root / "README.md"
    tags = ", ".join(f"`{s}`" for s in supported(source))
    docs, count = re.subn(r"^Supported image tags: .+$", f"Supported image tags: {tags}.",
                          readme.read_text(), flags=re.M)
    if count != 1:
        raise ValueError("Expected one supported image tag list in README.md")
    dockerfile.write_text(source)
    config_path.write_text(json.dumps(config, indent=2) + "\n")
    readme.write_text(docs)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    commands.add_parser("matrix")
    commands.add_parser("discover")
    add = commands.add_parser("add")
    for name in ("series", "version", "digest"):
        add.add_argument(name)
    args = parser.parse_args()
    if args.command == "add":
        add_series(Path("."), args.series, args.version, args.digest)
    else:
        series = supported(Path("Dockerfile").read_text())
        print(json.dumps(series if args.command == "matrix" else discover(series)))
