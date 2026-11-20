"""Publish verified build outputs as a GitHub release and check the public feeds."""
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile


PACKAGES = {"macos_arm64": "macos-arm64.dmg", "windows_x64": "windows-x64-setup.exe",
            "windows_arm64": "windows-arm64-setup.exe", "linux_x64": "linux-x64.deb",
            "linux_arm64": "linux-arm64.deb", "android_arm64": "android-arm64.apk"}
# Packages offered beside the installers, without an update feed of their own.
EXTRAS = ["linux-x64.AppImage", "linux-arm64.AppImage"]


def gh(*args):
    return subprocess.check_output(["gh", *args], text=True).strip()


def version(tag):
    match = re.fullmatch(r"v(\d+)\.(\d+)\.(\d+)", tag)
    if not match:
        raise ValueError(f"Expected a stable version tag, got {tag!r}")
    return tuple(map(int, match.groups()))


def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def release_assets(directory, tag):
    files = {f"appcast-{target}.xml" for target in PACKAGES}
    files.update(f"remote-go-{tag[1:]}-{suffix}" for suffix in [*PACKAGES.values(), *EXTRAS])
    expected = files | {f"{name}.sha256" for name in files}
    if {p.name for p in directory.iterdir()} != expected:
        raise ValueError(f"Expected {len(PACKAGES)} installers and feeds and {len(EXTRAS)} AppImages, each with a checksum file")
    for name in files:
        checksum, filename = (directory / (name + ".sha256")).read_text().split()
        if filename != name or checksum != digest(directory / name):
            raise ValueError(f"Invalid checksum for {name}")
    return sorted(directory / name for name in expected)


def publish(directory, tag, repo):
    requested = version(tag)
    assets = release_assets(directory, tag)
    # Do not let a delayed build or rerun move the public update channel backwards.
    latest = gh("api", f"repos/{repo}/releases/latest", "--jq", ".tag_name")
    if version(latest) > requested:
        raise ValueError(f"Latest is already {latest}")
    if latest != tag:
        # Reuse an interrupted draft. Never replace assets of a published release.
        found = subprocess.run(["gh", "release", "view", tag, "-R", repo, "--json", "isDraft"],
                               capture_output=True, text=True)
        if found.returncode:
            gh("release", "create", tag, "-R", repo, "--draft", "--verify-tag",
               "--title", f"Remote Go {tag[1:]}", "--generate-notes")
        elif not json.loads(found.stdout)["isDraft"]:
            raise ValueError(f"{tag} is already published")
        gh("release", "upload", tag, "-R", repo, "--clobber", *map(str, assets))
        uploaded = json.loads(gh("release", "view", tag, "-R", repo, "--json", "assets"))["assets"]
        expected = {p.name: "sha256:" + digest(p) for p in assets}
        if {a["name"]: a["digest"] for a in uploaded} != expected:
            raise ValueError("Uploaded draft differs from verified build outputs")
        gh("release", "edit", tag, "-R", repo, "--draft=false", "--latest")
    if gh("api", f"repos/{repo}/releases/latest", "--jq", ".tag_name") != tag:
        raise ValueError(f"{tag} is not Latest")
    # On a rerun the released packages may have different signing timestamps.
    # Verify the public release against its own signed feeds and checksums.
    with tempfile.TemporaryDirectory() as tmp:
        online = Path(tmp)
        for p in assets:
            endpoint = "latest/download" if p.name.startswith("appcast-") else f"download/{tag}"
            url = f"https://github.com/{repo}/releases/{endpoint}/{p.name}"
            subprocess.run(["curl", "-fsSL", "--retry", "5", "--retry-all-errors",
                            "--max-time", "180", "-o", online / p.name, url], check=True)
        release_assets(online, tag)
        subprocess.run(["go", "run", "./scripts/verify-feed", online, *PACKAGES], check=True)


if __name__ == "__main__":
    publish(Path(sys.argv[1]), sys.argv[2], os.environ["GITHUB_REPOSITORY"])
