"""Compatibility entry point for building the Android controller."""
import os
from pathlib import Path
import sys

os.execv(sys.executable, [sys.executable, str(Path(__file__).with_name("build.py")), "android"])
