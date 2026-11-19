"""Build Remote Go with its native screen/clipboard support and signed update feeds."""
import argparse
import os
from pathlib import Path
import platform
import shutil
import subprocess
import tempfile
import zipfile


ROOT = Path(__file__).resolve().parents[1]
TARGETS = {"mac": ["macos_arm64"], "windows": ["windows_x64", "windows_arm64"],
           "android": ["android_arm64"]}
# The certificate used by the APKs already distributed to Android users.
APK_CERT = "c71035aa07415dcfb3c145b9fc946a2a00fd250c300708997a4ba357536e0878"


def run(*args, **kw):
    subprocess.run(args, check=True, **kw)


def sdk_tool(pattern):
    sdk = Path(os.getenv("ANDROID_HOME") or os.getenv("ANDROID_SDK_ROOT") or
               (Path.home() / ("Library/Android/sdk" if platform.system() == "Darwin" else "Android/Sdk")))
    candidates = list(sdk.glob(pattern))
    if not candidates:
        raise SystemExit(f"Android SDK missing {pattern} under {sdk}")
    return max(candidates, key=lambda p: tuple(int(v) for v in
               p.parent.name.removeprefix("android-").split(".") if v.isdigit()))


def patch(path, old, new):
    src = path.read_text()
    if src.count(old) != 1:
        raise SystemExit(f"Review NSGo packaging hook in {path.name}")
    path.unlink()
    path.write_text(src.replace(old, new))


def android_runtime(packager, tmp):
    android = packager / "internal/platform/android"
    sdk_jar = sdk_tool("platforms/android-*/android.jar")
    classes = tmp / "classes"
    classes.mkdir()
    jar = tmp / "runtime.jar"
    shutil.copyfile(android / "runtime/classes.jar", jar)
    provider = android / "java/dev/oneo/nsgo/FileProvider.java"
    patch(provider, "public boolean onCreate() {",
          "public boolean onCreate() {\n        ClipboardBridge.start(getContext());")
    run("javac", "--release", "17", "-classpath", os.pathsep.join(map(str, [sdk_jar, jar])),
        "-d", classes, provider, "internal/mobile/android/ClipboardBridge.java")
    run("jar", "uf", jar, "-C", classes, ".")
    run(sdk_tool("build-tools/*/d8"), "--lib", sdk_jar, "--min-api", "28", "--output", tmp, jar)
    dex = android / "runtime/classes.dex"
    dex.unlink()
    shutil.copyfile(tmp / "classes.dex", dex)
    return dex


def verify_output(directory, target, version, signed, dex):
    releases = directory / "releases"
    extension = {"macos": ".dmg", "windows": "-setup.exe", "android": ".apk"}[target.split("_")[0]]
    name = f"remote-go-{version}-{target.replace('_', '-')}{extension}"
    package = releases / name
    if not package.is_file():
        raise SystemExit(f"Missing installer: {name}")
    if target == "android_arm64":
        with zipfile.ZipFile(package) as apk:
            if apk.read("classes.dex") != dex.read_bytes():
                raise SystemExit("Missing Android clipboard extension")
        certs = subprocess.check_output([sdk_tool("build-tools/*/apksigner"), "verify",
                                         "--print-certs", package], text=True)
        if signed and f"Signer #1 certificate SHA-256 digest: {APK_CERT}" not in certs.splitlines():
            raise SystemExit("Android signing certificate differs from previous releases")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("platform", choices=[*TARGETS, "release"], default="mac", nargs="?")
    args = parser.parse_args()
    os.chdir(ROOT)
    targets = (sum(TARGETS.values(), []) if args.platform == "release" else TARGETS[args.platform])
    if "windows_x64" in targets and not shutil.which("makensis"):
        raise SystemExit("Install NSIS (makensis) to build Windows setup packages")
    version = next(line.split()[1] for line in Path("nsgo.yaml").read_text().splitlines()
                   if line.startswith("version:"))
    env = os.environ.copy()
    keyfile = Path.home() / ".config/remote-go/updates.key"
    if not env.get("NSGO_UPDATES_KEY") and keyfile.is_file():
        env["NSGO_UPDATES_KEY"] = keyfile.read_text().strip()
    signed = bool(env.get("NSGO_UPDATES_KEY"))
    if not signed and args.platform == "release":
        raise SystemExit("Set NSGO_UPDATES_KEY to sign a release")
    # An unsigned local build never overwrites assets intended for publishing.
    group = "signed" if args.platform == "release" else "build-" + args.platform
    directory = ROOT / "bin" / group / version
    directory.parent.mkdir(parents=True, exist_ok=True)
    module = Path(subprocess.check_output(["go", "list", "-m", "-f", "{{.Dir}}",
                                          "github.com/nsgo-dev/nsgo"], text=True).strip())
    with tempfile.TemporaryDirectory(dir=directory.parent, prefix=".building-") as scratch:
        tmp = Path(scratch)
        packager = tmp / "nsgo"
        shutil.copytree(module, packager)
        for folder, _, _ in os.walk(packager):
            Path(folder).chmod(0o755)
        if "macos_arm64" in targets:
            # Use the original desktop bundle identity and CGO screen/input implementation.
            source = packager / "cmd/nsgo/build.go"
            patch(source, 'app := filepath.Join(dir, p.Name+".app")',
                  'p.ID = "dev.remote-go.app"\n\tapp := filepath.Join(dir, p.Name+".app")')
            patch(source, '"GOOS=darwin", "GOARCH=" + arch, "CGO_ENABLED=0"',
                  '"GOOS=darwin", "GOARCH=" + arch, "CGO_ENABLED=1"')
            patch(source, '<key>LSMinimumSystemVersion</key><string>11.0</string>',
                  '<key>LSMinimumSystemVersion</key><string>12.3</string>')
        dex = android_runtime(packager, tmp) if "android_arm64" in targets else None
        stage = tmp / "output"
        tool = tmp / "nsgo-tool"
        run("go", "-C", packager, "build", "-o", tool, "./cmd/nsgo")
        run(tool, "build", "-target", ",".join(targets), "-output", stage, ".", env=env)
        for target in targets:
            verify_output(stage, target, version, signed, dex)
        if signed:
            run("go", "run", "./scripts/verify-feed", stage / "releases", *targets)
        # Expose the complete set only after every selected target passes verification.
        previous = tmp / "previous"
        if directory.exists():
            directory.rename(previous)
        try:
            stage.rename(directory)
        except OSError:
            if previous.exists():
                previous.rename(directory)
            raise
    print(directory / "releases")


if __name__ == "__main__":
    main()
