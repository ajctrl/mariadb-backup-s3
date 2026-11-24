"""Refresh only the bot-owned Dockerfile pin in an open LTS proposal."""

import base64
import json
import os
from pathlib import Path
import re
import subprocess

from mariadb_lts import DIGEST, STAGE, add_series


def api(*args, payload=None):
    if payload is not None:
        args = (*args, "--input", "-")
    body = json.dumps(payload) if payload is not None else None
    return json.loads(subprocess.check_output(["gh", "api", *args], text=True, input=body))


def refreshed_dockerfile(file, ranges, series, version, digest, bot):
    if not re.fullmatch(re.escape(series) + r"\.[0-9]+", version) or not re.fullmatch(DIGEST, digest):
        raise ValueError("Invalid candidate image")
    matches = [m for m in STAGE.finditer(file) if m[1] == series]
    if len(matches) != 1:
        return None
    match = matches[0]
    line = file.count("\n", 0, match.start()) + 1
    owner = next((r["commit"]["author"]["user"] for r in ranges
                  if r["startingLine"] <= line <= r["endingLine"]), None)
    if not owner or owner["login"] != bot:
        return None
    old_version = match[0].split(":", 1)[1].split("@", 1)[0]
    new_stage = f"FROM mariadb:{version}@{digest} AS client-{series}"
    if int(version.split(".")[-1]) < int(old_version.split(".")[-1]) or new_stage == match[0]:
        return None
    return file[:match.start()] + new_stage + file[match.end():]


# Read the file and line authors from exactly the commit we will compare-and-swap.
READ = """
query($owner:String!, $name:String!, $head:GitObjectID!, $path:String!) {
  repository(owner:$owner, name:$name) {
    file:object(expression:$path) { ... on Blob { text } }
    head:object(oid:$head) { ... on Commit {
      blame(path:"Dockerfile") { ranges { startingLine endingLine commit { author { user { login } } } } }
    } }
  }
}
"""
COMMIT = """
mutation($input:CreateCommitOnBranchInput!) {
  createCommitOnBranch(input:$input) { commit { oid } }
}
"""


def prepare(repo, branch, series, version, digest, bot):
    owner, name = repo.split("/")
    prs = api("--method", "GET", f"repos/{repo}/pulls", "-f", "base=main",
              "-f", f"head={owner}:{branch}", "-f", "state=all")
    if not prs:
        add_series(Path("."), series, version, digest)
        return True
    pr = next((p for p in prs if p["state"] == "open"), None)
    if not pr or pr["user"]["login"] != bot or pr["head"]["repo"]["full_name"] != repo:
        return False
    head = pr["head"]["sha"]
    data = api("graphql", payload={"query": READ, "variables": {
        "owner": owner, "name": name, "head": head, "path": head + ":Dockerfile",
    }})["data"]["repository"]
    if not data["file"] or not data["file"]["text"]:
        return False
    updated = refreshed_dockerfile(data["file"]["text"], data["head"]["blame"]["ranges"],
                                   series, version, digest, bot)
    if updated is not None:
        api("graphql", payload={"query": COMMIT, "variables": {"input": {
            "branch": {"repositoryNameWithOwner": repo, "branchName": branch},
            "expectedHeadOid": head,
            "message": {"headline": f"Update MariaDB {series} LTS proposal to {version} ({digest[:19]})"},
            "fileChanges": {"additions": [{"path": "Dockerfile",
                                           "contents": base64.b64encode(updated.encode()).decode()}]},
        }}})
    return False


if __name__ == "__main__":
    env = os.environ
    create = prepare(env["GITHUB_REPOSITORY"], env["BRANCH"], env["SERIES"],
                     env["VERSION"], env["DIGEST"], env["BOT_LOGIN"])
    with open(env["GITHUB_OUTPUT"], "a") as output:
        print("create=" + str(create).lower(), file=output)
