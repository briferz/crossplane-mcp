#!/usr/bin/env python3
"""Fail when pins that must move together have drifted apart.

Dependabot moves one half of a coupled pair; nothing moves the other, and
nothing fails when they diverge — the affected job keeps passing while quietly
testing something other than what ships. That happened three times in one week
here: the Dockerfile without the Go toolchain, the toolchain without
golangci-lint, and client-go without envtest.

Each check below encodes a pair, not a preference. Deliberate divergence is a
one-line edit to this file, which is the point: it becomes a decision rather
than an accident.
"""

import pathlib
import re
import sys

ROOT = pathlib.Path(__file__).resolve().parents[2]


def read(rel: str) -> str:
    return (ROOT / rel).read_text()


def check_envtest_tracks_client_go(problems: list[str]) -> None:
    """ENVTEST_K8S_VERSION must match the client-go minor it is tested against."""
    cg = re.search(r"k8s\.io/client-go v0\.(\d+)\.\d+", read("go.mod"))
    ev = re.search(r"^ENVTEST_K8S_VERSION \?= 1\.(\d+)\.", read("Makefile"), re.M)
    if not cg or not ev:
        problems.append("could not read the client-go or ENVTEST_K8S_VERSION pin")
        return
    if cg.group(1) != ev.group(1):
        problems.append(
            f"client-go is v0.{cg.group(1)}.x but ENVTEST_K8S_VERSION is 1.{ev.group(1)}.x — "
            "the integration tier is testing a different apiserver than the client targets"
        )


def check_envtest_cache_key_matches(problems: list[str]) -> None:
    """The ci.yml cache key must name the version the Makefile actually uses."""
    ev = re.search(r"^ENVTEST_K8S_VERSION \?= (\S+)", read("Makefile"), re.M)
    key = re.search(r"key: envtest-\$\{\{ runner\.os \}\}-(\S+)", read(".github/workflows/ci.yml"))
    if not ev or not key:
        problems.append("could not read ENVTEST_K8S_VERSION or the envtest cache key")
        return
    if ev.group(1) != key.group(1):
        problems.append(
            f"ENVTEST_K8S_VERSION is {ev.group(1)} but the ci.yml cache key says {key.group(1)} — "
            "the cache would be restored for the wrong apiserver"
        )


def check_docker_go_matches_toolchain(problems: list[str]) -> None:
    """The Dockerfile's golang image must not lag the go.mod toolchain.

    `toolchain` is a floor, not a pin: a NEWER image is used as-is, so an image
    behind the floor makes the container download a toolchain mid-build while
    the release binaries (setup-go, go-version-file) use the floor. The two
    artifacts then ship built by different compilers.
    """
    tc = re.search(r"^toolchain go(\d+)\.(\d+)\.", read("go.mod"), re.M)
    img = re.search(r"^FROM .*golang:(\d+)\.(\d+)", read("Dockerfile"), re.M)
    if not tc or not img:
        problems.append("could not read the go.mod toolchain or the Dockerfile golang image")
        return
    if (tc.group(1), tc.group(2)) != (img.group(1), img.group(2)):
        problems.append(
            f"go.mod toolchain is go{tc.group(1)}.{tc.group(2)}.x but the Dockerfile uses "
            f"golang:{img.group(1)}.{img.group(2)} — the image and the release binaries would be "
            "built by different Go versions"
        )


def main() -> int:
    problems: list[str] = []
    check_envtest_tracks_client_go(problems)
    check_envtest_cache_key_matches(problems)
    check_docker_go_matches_toolchain(problems)

    if problems:
        print("coupled version pins have drifted:\n", file=sys.stderr)
        for p in problems:
            print(f"  {p}", file=sys.stderr)
        print(
            "\nMove them together, or update .github/scripts/check-version-pins.py if the\n"
            "divergence is deliberate.",
            file=sys.stderr,
        )
        return 1

    print("coupled version pins agree")
    return 0


if __name__ == "__main__":
    sys.exit(main())
