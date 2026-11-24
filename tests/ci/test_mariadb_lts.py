import base64
from contextlib import chdir
import json
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / ".github/scripts"))
import mariadb_lts as lts
import lts_proposal as proposal


def release(series, status="Stable", support="Long Term Support", eol="2099-01-01"):
    return {"release_id": series, "release_status": status,
            "release_support_type": support, "release_eol_date": eol}


def tag(version, archs=("amd64", "arm64")):
    return {"name": version, "digest": "sha256:" + "a" * 64,
            "images": [{"os": "linux", "architecture": arch} for arch in archs]}


class LtsDiscovery(unittest.TestCase):
    def test_matrix_requires_the_default_and_all_documented_series(self):
        image = "sha256:" + "a" * 64
        default = "ARG MARIADB_SERIES=11.4\n"
        first = f"FROM mariadb:11.4.1@{image} AS client-11.4\n"
        second = f"FROM mariadb:11.8.1@{image} AS client-11.8\n"
        docs = "Supported image tags: `11.4`, `11.8`.\n"
        self.assertEqual(lts.series_matrix(default + first + second, docs), ["11.4", "11.8"])
        for dockerfile, readme, error in [
            (default + second, "Supported image tags: `11.8`.\n", "default MARIADB_SERIES"),
            (first + default + second, docs, "default MARIADB_SERIES"),
            (default + first, docs, "must match"),
            (default + first + second, "Supported image tags: `11.4`.\n", "must match"),
            (default + first + second, docs + docs, "one supported image tag list"),
        ]:
            with self.subTest(dockerfile=dockerfile, readme=readme), self.assertRaisesRegex(ValueError, error):
                lts.series_matrix(dockerfile, readme)

    def test_patch_candidates_stay_in_each_supported_series(self):
        config = json.loads((ROOT / "renovate.json").read_text())
        for series in lts.supported((ROOT / "Dockerfile").read_text()):
            rule, = (r for r in config["packageRules"] if r.get("groupName") == f"MariaDB {series}")
            pattern = rule["allowedVersions"].strip("/")
            self.assertRegex(series + ".14", pattern)
            major, minor = map(int, series.split("."))
            self.assertNotRegex(f"{major}.{minor + 1}.1", pattern)
            self.assertNotRegex(series + ".14-rc", pattern)

    def test_discover_add_and_repeat(self):
        existing = lts.supported((ROOT / "Dockerfile").read_text())
        # Pick an unregistered series even after real series have been added.
        new = str(max(int(s.split(".")[0]) for s in existing) + 1)
        series = new + ".3"
        catalog = {"major_releases": [
            release(existing[0]), release(series), release(new + ".4", "RC"),
            release(new + ".0", support="Rolling"), release("5.5", eol="2020-01-01"),
        ]}
        first = {"results": [tag(series + ".11", ("amd64",)), tag(series + ".9"),
                             tag(series + ".20-rc"), tag(series + ".20-ubi")], "next": "page2"}
        second = {"results": [tag(series + ".10")], "next": None}
        with patch.object(lts, "fetch", side_effect=[catalog, first, second]) as fetch:
            candidates = lts.discover(existing)
        self.assertEqual(fetch.call_count, 3)
        entry = tag(series + ".10")
        self.assertEqual(candidates, [{"series": series, "version": entry["name"], "digest": entry["digest"]}])

        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            files = [root / name for name in ("Dockerfile", "README.md", "renovate.json")]
            for path in files:
                shutil.copyfile(ROOT / path.name, path)
            lts.add_series(root, **candidates[0])
            contents = [p.read_text() for p in files]
            self.assertEqual(lts.series_matrix(contents[0], contents[1]), existing + [series])
            self.assertIn(f"mariadb:{entry['name']}@{entry['digest']} AS client-{series}", contents[0])
            self.assertIn(f"`{series}`", contents[1])
            rule = json.loads(contents[2])["packageRules"][-1]
            self.assertEqual(rule["groupName"], "MariaDB " + series)
            self.assertRegex(entry["name"], rule["matchCurrentVersion"].strip("/"))
            self.assertRegex(entry["name"], rule["allowedVersions"].strip("/"))
            self.assertNotRegex(new + ".4.1", rule["allowedVersions"].strip("/"))
            lts.add_series(root, **candidates[0])
            self.assertEqual(contents, [p.read_text() for p in files])
            with patch.object(lts, "fetch", return_value=catalog) as fetch:
                self.assertEqual(lts.discover(lts.supported(contents[0])), [])
                fetch.assert_called_once_with(lts.FOUNDATION)

    def test_wait_for_official_multiarch_image(self):
        catalog = {"major_releases": [release("99.3")]}
        for tags in ([], [tag("99.3.2", ("amd64",))], [tag("99.3.2-rc")]):
            with self.subTest(tags=tags), patch.object(lts, "fetch", side_effect=[
                catalog, {"results": tags, "next": None},
            ]):
                self.assertEqual(lts.discover([]), [])


class LtsProposal(unittest.TestCase):
    repo = "example/backup"
    bot = "lts-app[bot]"
    old = "sha256:" + "a" * 64
    new = "sha256:" + "b" * 64

    def test_existing_branch_is_preserved_without_a_pr(self):
        branch = "lts/mariadb-99.3"
        head = "1" * 40
        refs = [{"ref": f"refs/heads/{branch}", "object": {"sha": head}}]
        title = "feat: support MariaDB 99.3 LTS"
        for author, verified, message, open_pr in [
            ("maintainer", True, title, False), (self.bot, False, title, False),
            (self.bot, True, "other work", False), (self.bot, True, title, True),
        ]:
            commit = {"author": {"login": author}, "commit": {
                "verification": {"verified": verified}, "message": message,
            }}
            with self.subTest(author=author, verified=verified, message=message), patch.object(
                proposal, "api", side_effect=[[], refs, commit]
            ) as api:
                self.assertEqual(proposal.prepare(self.repo, branch, "99.3", "99.3.2", self.new, self.bot), open_pr)
                self.assertEqual(api.call_count, 3)
                self.assertEqual(api.call_args_list[1].args, (f"repos/{self.repo}/git/matching-refs/heads/{branch}",))
                api.assert_called_with(f"repos/{self.repo}/commits/{head}")

    def test_first_proposal_atomically_creates_its_branch(self):
        branch = "lts/mariadb-99.3"
        ref = "refs/heads/" + branch
        git_api = f"repos/{self.repo}/git"
        sibling = {"ref": ref + "0", "object": {"sha": "1" * 40}}
        concurrent = {"ref": ref, "object": {"sha": "2" * 40}}
        commit = {"sha": "3" * 40, "verification": {"verified": True}}
        failure = subprocess.CalledProcessError(1, ["gh", "api"])
        for case, responses, expected in [
            ("created", [{}], True),
            ("collision", [failure, [concurrent]], False),
            ("unrelated failure", [failure, []], None),
        ]:
            with self.subTest(case=case), tempfile.TemporaryDirectory() as tmp, chdir(tmp):
                paths = ("Dockerfile", "renovate.json", "README.md")
                for path in paths:
                    shutil.copyfile(ROOT / path, path)
                with patch.object(proposal, "api", side_effect=[
                    [], [sibling], {"sha": "new-tree"}, commit, *responses,
                ]) as api, patch.object(subprocess, "check_output", return_value="base\nbase-tree\n"):
                    args = (self.repo, branch, "99.3", "99.3.2", self.new, self.bot)
                    if expected is None:
                        with self.assertRaises(subprocess.CalledProcessError):
                            proposal.prepare(*args)
                    else:
                        self.assertEqual(proposal.prepare(*args), expected)
                writes = [(c.args, c.kwargs["payload"]) for c in api.call_args_list if "payload" in c.kwargs]
                self.assertEqual(writes, [
                    ((git_api + "/trees",), {"base_tree": "base-tree", "tree": [
                        {"path": p, "mode": "100644", "type": "blob", "content": Path(p).read_text()} for p in paths
                    ]}),
                    ((git_api + "/commits",), {"message": "feat: support MariaDB 99.3 LTS",
                                               "parents": ["base"], "tree": "new-tree"}),
                    ((git_api + "/refs",), {"ref": ref, "sha": commit["sha"]}),
                ])

    def test_open_proposal_refreshes_only_its_pin_at_the_read_head(self):
        pull = {"state": "open", "user": {"login": self.bot},
                "head": {"repo": {"full_name": self.repo}, "sha": "1" * 40}}
        source = f"# maintainer edit\nFROM mariadb:99.3.2@{self.old} AS client-99.3\n"
        for author, version, digest, write in [
            (self.bot, "99.3.3", self.new, True), (self.bot, "99.3.2", self.new, True),
            (self.bot, "99.3.2", self.old, False), ("maintainer", "99.3.3", self.new, False),
            (self.bot, "99.3.1", self.new, False),
        ]:
            ranges = [{"startingLine": 2, "endingLine": 2, "commit": {"author": {"user": {"login": author}}}}]
            data = {"file": {"text": source}, "head": {"blame": {"ranges": ranges}}}
            with self.subTest(author=author, version=version, digest=digest), patch.object(
                proposal, "api", side_effect=[[pull], {"data": {"repository": data}}, {}]
            ) as api:
                self.assertFalse(proposal.prepare(self.repo, "lts/mariadb-99.3", "99.3", version, digest, self.bot))
                self.assertEqual(api.call_count, 3 if write else 2)
                if write:
                    commit = api.call_args.kwargs["payload"]["variables"]["input"]
                    self.assertEqual(commit["expectedHeadOid"], pull["head"]["sha"])
                    change, = commit["fileChanges"]["additions"]
                    self.assertEqual(change["path"], "Dockerfile")
                    expected = source.replace("99.3.2@" + self.old, version + "@" + digest)
                    self.assertEqual(base64.b64decode(change["contents"]).decode(), expected)

    def test_closed_proposal_stays_closed(self):
        with patch.object(proposal, "api", return_value=[{"state": "closed"}]) as api:
            self.assertFalse(proposal.prepare(self.repo, "lts/mariadb-99.3", "99.3", "99.3.2", self.new, self.bot))
            api.assert_called_once()


if __name__ == "__main__":
    unittest.main()
