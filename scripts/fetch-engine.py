#!/usr/bin/env python3
"""Fetch FreeToken's nightly engine wheel pair, verified, for the Dockerfile.

    fetch-engine.py <dest-dir>

Upstream publishes each new build of main to a rolling `nightly` release, beside
a manifest -- engine-linux_x86_64.json -- naming the runtime wheel, the
kernel-cache wheel built with it, and their sha256 and sizes. This resolves the
manifest, checks it, and downloads the pair into a directory that must not
already exist:

    <dest>/runtime/<runtime wheel>
    <dest>/kernel-cache/<kernel-cache wheel>
    <dest>/engine.json      the manifest, kept as the record of what was fetched

It installs nothing. Whether the kernel cache suits the torch that pip resolves
for the runtime can only be decided once the runtime is in, so that is the
Dockerfile's job.

Environment:
    FREETOKEN_COMMIT        fail unless nightly is built from this commit
    FREETOKEN_MANIFEST_URL  read the manifest from here instead (https or file)

FREETOKEN_COMMIT cannot pin. Every publish deletes the previous wheels before
uploading the new ones, so a commit nightly has moved past is gone for good.
What it can do is turn "the engine changed under you" into a failed build.

Standard library only: it runs before anything is installed.
"""

import hashlib
import json
import os
import re
import sys
import urllib.parse
import urllib.request

DEFAULT_MANIFEST = (
    "https://github.com/FlashML-org/FreeToken/releases/download/nightly/engine-linux_x86_64.json"
)

# The names become file paths and a pip argument, so they are held to the exact
# shape upstream publishes rather than trusted: stamp is the commit, py the
# CPython the runtime is built for, cuda the CUDA build of the cache.
RUNTIME_NAME = re.compile(
    r"freetoken-[0-9][0-9A-Za-z.]*\+g(?P<stamp>[0-9a-f]{7,40})-(?P<py>cp3[0-9]+)-cp3[0-9]+-linux_x86_64\.whl"
)
CACHE_NAME = re.compile(
    r"freetoken_kernel_cache-[0-9][0-9A-Za-z.]*\+(?P<cuda>cu[0-9]+)\.g(?P<stamp>[0-9a-f]{7,40})-py3-none-linux_x86_64\.whl"
)
COMMIT = re.compile(r"[0-9a-f]{7,40}")


def fail(msg):
    print(f"fetch-engine: {msg}", file=sys.stderr)
    sys.exit(1)


def same_commit(a, b):
    # Short and full shas name the same commit when one prefixes the other.
    return a.startswith(b) or b.startswith(a)


def open_url(url):
    scheme = urllib.parse.urlsplit(url).scheme
    if scheme not in ("https", "file"):
        fail(f"refusing to fetch {url!r}: only https and file URLs are allowed")
    try:
        return urllib.request.urlopen(url, timeout=60)
    except OSError as e:
        fail(f"fetching {url}: {e}")


def read_manifest(url):
    with open_url(url) as r:
        body = r.read((1 << 20) + 1)
    if len(body) > 1 << 20:
        fail(f"manifest at {url} is over 1 MiB; that is not an engine manifest")
    try:
        m = json.loads(body)
    except ValueError as e:
        fail(f"manifest at {url} is not JSON: {e}")
    if not isinstance(m, dict):
        fail(f"manifest at {url} is not a JSON object")
    if m.get("schema") != 1:
        fail(f"manifest schema is {m.get('schema')!r}; this script reads schema 1")
    if m.get("platform") != "linux_x86_64":
        fail(f"manifest is for {m.get('platform')!r}, not linux_x86_64")
    if not isinstance(m.get("commit"), str) or not COMMIT.fullmatch(m["commit"]):
        fail(f"manifest commit {m.get('commit')!r} is not a commit hash")
    return m


def wheel(m, key, pattern):
    e = m.get(key)
    if not isinstance(e, dict):
        fail(f"manifest has no {key} entry")
    name, url, sha, size = e.get("name"), e.get("url"), e.get("sha256"), e.get("size")
    match = pattern.fullmatch(name) if isinstance(name, str) else None
    if not match:
        fail(f"{key} wheel name {name!r} is not the shape expected; has upstream changed its naming?")
    # The engine refuses a runtime and cache whose stamps differ, but only when
    # it starts. Checked here, the same mistake fails the build instead.
    if not same_commit(match.group("stamp"), m["commit"]):
        fail(f"{name} is stamped g{match.group('stamp')}, but the manifest says it is {m['commit']}")
    if not isinstance(url, str) or urllib.parse.unquote(url.rsplit("/", 1)[-1]) != name:
        fail(f"{key} URL {url!r} does not name {name}")
    if not isinstance(sha, str) or not re.fullmatch(r"[0-9a-f]{64}", sha):
        fail(f"{key} sha256 {sha!r} is not a sha256")
    if not isinstance(size, int) or isinstance(size, bool) or size <= 0:
        fail(f"{key} size {size!r} is not a byte count")
    return name, url, sha, size, match


def download(url, path, sha, size):
    h = hashlib.sha256()
    n = 0
    part = path + ".part"
    with open_url(url) as r, open(part, "wb") as f:
        while chunk := r.read(1 << 20):
            n += len(chunk)
            if n > size:
                break
            h.update(chunk)
            f.write(chunk)
    problem = None
    if n > size:
        problem = f"is larger than the {size} bytes the manifest says"
    elif n != size:
        problem = f"is {n} bytes, not the {size} the manifest says"
    elif h.hexdigest() != sha:
        problem = f"has sha256 {h.hexdigest()}, not the manifest's {sha}"
    if problem:
        os.unlink(part)
        fail(f"{url} {problem}")
    os.replace(part, path)


def main(argv):
    if len(argv) != 2:
        print("usage: fetch-engine.py <dest-dir>", file=sys.stderr)
        return 2
    dest = argv[1]

    m = read_manifest(os.environ.get("FREETOKEN_MANIFEST_URL") or DEFAULT_MANIFEST)
    commit = m["commit"]

    want = os.environ.get("FREETOKEN_COMMIT", "").strip().lower()
    if want:
        if not COMMIT.fullmatch(want):
            fail(f"FREETOKEN_COMMIT={want!r} is not a 7-40 hex commit")
        if not same_commit(want, commit):
            fail(
                f"nightly is built from {commit}, not the pinned {want}. Nightly keeps only "
                f"its latest build, so {want} can no longer be fetched: pin {commit} or unset "
                "FREETOKEN_COMMIT."
            )

    runtime = wheel(m, "runtime", RUNTIME_NAME)
    cache = wheel(m, "kernel_cache", CACHE_NAME)

    # The Dockerfile compares the manifest's cuda with torch's before installing
    # the cache, so it has to be the build the cache wheel really is.
    if m.get("cuda") != cache[4].group("cuda"):
        fail(f"manifest says cuda {m.get('cuda')!r}, but {cache[0]} is built for {cache[4].group('cuda')}")

    # The runtime is built for one CPython. Without this a mismatch surfaces as
    # pip's "not a supported wheel on this platform", which does not say why.
    here = f"cp{sys.version_info.major}{sys.version_info.minor}"
    if runtime[4].group("py") != here:
        fail(f"nightly's runtime is built for {runtime[4].group('py')}, but this is {here}")

    try:
        os.makedirs(os.path.join(dest, "runtime"))
        os.makedirs(os.path.join(dest, "kernel-cache"))
    except FileExistsError:
        fail(f"{dest} already exists; give a fresh directory so it holds exactly one pair")

    for sub, (name, url, sha, size, _) in (("runtime", runtime), ("kernel-cache", cache)):
        download(url, os.path.join(dest, sub, name), sha, size)
    with open(os.path.join(dest, "engine.json"), "w") as f:
        json.dump(m, f, indent=2)
        f.write("\n")

    print(f"fetch-engine: nightly {m.get('version')} (commit {commit}, {here}, {m.get('cuda')}), sha256 verified")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
