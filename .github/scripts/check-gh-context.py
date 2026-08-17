#!/usr/bin/env python3
"""Fail if a workflow job calls `gh` without giving it a repository to act on.

`gh` resolves the target repo from the git remote. A job that runs no
actions/checkout has no remote, so every invocation dies with:

    failed to run git: fatal: not a git repository

That is not theoretical. Both failure notifiers in this repo carried exactly
that defect from the day they were written until 2026-08-17, and neither ever
ran to completion — including during the scheduled run that found four stdlib
CVEs. The bug is invisible in review because the YAML looks complete, and
invisible in CI because the jobs only run on failure.

This is a cheap guard for one specific mistake, NOT proof the notifier works.
It cannot catch a missing `issues: write`, a token-scope change, or a `gh`
behaviour change. Only firing the notifier catches those, which is what the
self-test in notify-failure.yml is for.
"""

import sys
import pathlib
import yaml

WORKFLOWS = pathlib.Path(__file__).resolve().parents[1] / "workflows"


def job_runs_gh(job: dict) -> bool:
    return any("gh " in (step.get("run") or "") for step in job.get("steps") or [])


def job_has_repo_context(job: dict) -> bool:
    """Either a checkout (providing a git remote) or an explicit GH_REPO."""
    steps = job.get("steps") or []
    if any("actions/checkout" in str(step.get("uses", "")) for step in steps):
        return True
    scopes = [job.get("env") or {}]
    scopes += [step.get("env") or {} for step in steps]
    return any("GH_REPO" in env for env in scopes)


def main() -> int:
    problems = []
    for path in sorted(WORKFLOWS.glob("*.yml")):
        doc = yaml.safe_load(path.read_text()) or {}
        for name, job in (doc.get("jobs") or {}).items():
            # A `uses:` job delegates to another workflow and has no steps of
            # its own; the callee is checked on its own pass.
            if not isinstance(job, dict) or "uses" in job:
                continue
            if job_runs_gh(job) and not job_has_repo_context(job):
                problems.append(f"{path.name}: job '{name}' runs gh with no checkout and no GH_REPO")

    if problems:
        print("gh invocations with no repository context:\n", file=sys.stderr)
        for p in problems:
            print(f"  {p}", file=sys.stderr)
        print(
            "\nAdd `GH_REPO: ${{ github.repository }}` to the job/step env, or an\n"
            "actions/checkout step. Without one, gh exits 1 before doing anything.",
            file=sys.stderr,
        )
        return 1

    print("all gh-using jobs have repository context")
    return 0


if __name__ == "__main__":
    sys.exit(main())
