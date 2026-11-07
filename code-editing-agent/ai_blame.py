#!/usr/bin/env python3
"""
ai_blame.py - show which lines in the current code were written by a given AI model.

Reads the provenance commits made by main.go (author = exact model version,
prompt stored in the commit body) and reports, for lines that still exist in HEAD:
  - file and line ranges
  - the diff hunks of the model's commit that produced those lines
  - the original prompt
  - later AI edits (with their prompts)

Examples:
  python ai_blame.py                                   # fully interactive
  python ai_blame.py --model claude-haiku-5-5-20261001 --since 2026-11-01 --out terminal
  python ai_blame.py --model claude-sonnet-5-5 --all --out file
  python ai_blame.py --model X --all --out file --file report.md --later file
"""
import argparse
import re
import subprocess
import sys
from collections import Counter, defaultdict
from datetime import datetime, timedelta

AI_EMAIL = "noreply@anthropic.com"  # must match aiAuthorEmail in main.go


# --------------------------------------------------------------------------
# git helpers
# --------------------------------------------------------------------------
def git(*args, check=True):
    p = subprocess.run(["git", *args], capture_output=True, text=True, encoding="utf-8", errors="replace")
    if check and p.returncode != 0:
        raise RuntimeError(f"git {' '.join(args)} failed: {p.stderr.strip()}")
    return p.stdout


def repo_root():
    try:
        return git("rev-parse", "--show-toplevel").strip()
    except RuntimeError:
        sys.exit("Not inside a git repository.")


def list_models():
    """Return {model_name: commit_count} for all AI-authored commits."""
    out = git("log", "--format=%an%x09%ae")
    c = Counter()
    for line in out.splitlines():
        if "\t" not in line:
            continue
        name, email = line.split("\t", 1)
        if email.strip() == AI_EMAIL:
            c[name] += 1
    return c


# --------------------------------------------------------------------------
# commit info / prompt parsing
# --------------------------------------------------------------------------
_commit_cache = {}


def parse_prompt(body):
    """Extract the indented block after a 'Prompt:' line in the commit body."""
    lines = body.splitlines()
    for i, line in enumerate(lines):
        if line.strip() == "Prompt:" and not line.startswith(" "):
            block = []
            for l in lines[i + 1:]:
                if l.startswith("    "):
                    block.append(l[4:])
                else:
                    break
            return "\n".join(block).strip()
    return ""


def commit_info(sha):
    if sha in _commit_cache:
        return _commit_cache[sha]
    out = git("show", "-s", "--format=%an%x1f%ae%x1f%aI%x1f%s%x1f%B", sha)
    name, email, date, subject, body = out.split("\x1f", 4)
    info = {
        "sha": sha,
        "short": sha[:8],
        "author": name,
        "email": email,
        "date": date,
        "subject": subject,
        "prompt": parse_prompt(body),
    }
    _commit_cache[sha] = info
    return info


# --------------------------------------------------------------------------
# finding the model's surviving lines
# --------------------------------------------------------------------------
def candidate_files(model):
    """Files touched by any commit authored by the model that still exist in HEAD."""
    out = git("log", f"--author={re.escape(model)}", "--format=@@%H\t%an", "--name-only", "--no-renames")
    files = set()
    for line in out.splitlines():
        if line.startswith("@@") or not line.strip():
            continue
        files.add(line.strip())
    in_head = set(git("ls-files").splitlines())
    return sorted(files & in_head)


HEADER_RE = re.compile(r"^([0-9a-f]{40}) (\d+) (\d+)(?: \d+)?$")


def blame_lines(path):
    """Yield dicts per line: final, orig, sha, author, time, filename."""
    out = git("blame", "--line-porcelain", "HEAD", "--", path, check=False)
    cur = None
    for line in out.splitlines():
        if line.startswith("\t"):
            if cur:
                yield cur
            cur = None
            continue
        m = HEADER_RE.match(line)
        if m:
            cur = {"sha": m.group(1), "orig": int(m.group(2)), "final": int(m.group(3)),
                   "author": "", "time": 0, "filename": path}
        elif cur is not None:
            if line.startswith("author "):
                cur["author"] = line[7:]
            elif line.startswith("author-time "):
                cur["time"] = int(line[12:])
            elif line.startswith("filename "):
                cur["filename"] = line[9:]


def find_ranges(model, files, start, end):
    """Return {file: [range dict]} with ranges of consecutive surviving lines by one commit."""
    result = {}
    for path in files:
        groups = []
        for ln in blame_lines(path):
            if ln["author"] != model:
                continue
            t = datetime.fromtimestamp(ln["time"])
            if start and t < start:
                continue
            if end and t > end:
                continue
            g = groups[-1] if groups else None
            if g and g["sha"] == ln["sha"] and g["end"] + 1 == ln["final"]:
                g["end"] = ln["final"]
                g["orig"].add(ln["orig"])
            else:
                groups.append({"sha": ln["sha"], "start": ln["final"], "end": ln["final"],
                               "orig": {ln["orig"]}, "filename": ln["filename"]})
        if groups:
            result[path] = groups
    return result


# --------------------------------------------------------------------------
# diff hunks and later edits
# --------------------------------------------------------------------------
HUNK_RE = re.compile(r"^@@ -\d+(?:,\d+)? \+(\d+)(?:,(\d+))? @@")


def relevant_hunks(sha, filename, orig_lines):
    """Hunks of the commit (for the file as named in that commit) overlapping the surviving lines."""
    out = git("show", "-U0", "--format=", sha, "--", filename, check=False)
    hunks, cur = [], None
    for line in out.splitlines():
        m = HUNK_RE.match(line)
        if m:
            if cur:
                hunks.append(cur)
            c = int(m.group(1))
            d = int(m.group(2)) if m.group(2) is not None else 1
            cur = {"lo": c, "hi": c + max(d, 1) - 1, "text": [line]}
        elif cur is not None:
            cur["text"].append(line)
    if cur:
        hunks.append(cur)
    return ["\n".join(h["text"]) for h in hunks if any(h["lo"] <= o <= h["hi"] for o in orig_lines)]


def later_ai_edits(path, rng, mode):
    """AI commits after the model's commit that touched the lines (mode=lines) or file (mode=file)."""
    fmt = "--format=%H"
    later = []
    if mode == "lines":
        out = git("log", "-L", f"{rng['start']},{rng['end']}:{path}", "-s", fmt, "HEAD", check=False)
        for sha in out.split():
            if sha == rng["sha"]:
                break
            later.append(sha)
    else:
        out = git("log", fmt, f"{rng['sha']}..HEAD", "--", path, check=False)
        later = out.split()
    infos = [commit_info(s) for s in later]
    return [i for i in infos if i["email"] == AI_EMAIL]


# --------------------------------------------------------------------------
# report
# --------------------------------------------------------------------------
def fence(text, lang=""):
    ticks = "```"
    while ticks in text:
        ticks += "`"
    return f"{ticks}{lang}\n{text}\n{ticks}"


def build_report(model, period, ranges_by_file, later_mode):
    total_lines = sum(r["end"] - r["start"] + 1 for rs in ranges_by_file.values() for r in rs)
    out = [f"# AI changes by `{model}`", "",
           f"- Period: {period}",
           f"- Generated: {datetime.now():%Y-%m-%d %H:%M}",
           f"- Files: {len(ranges_by_file)}",
           f"- Surviving lines: {total_lines}", ""]
    for path, rs in sorted(ranges_by_file.items()):
        n = sum(r["end"] - r["start"] + 1 for r in rs)
        out += [f"## `{path}` ({n} lines)", ""]
        for r in rs:
            info = commit_info(r["sha"])
            span = f"{r['start']}" if r["start"] == r["end"] else f"{r['start']}-{r['end']}"
            out += [f"### Lines {span} - commit `{info['short']}` ({info['date'][:16].replace('T', ' ')})", "",
                    f"_{info['subject']}_", "", "**Prompt:**", "",
                    fence(info["prompt"] or "(no prompt found in commit message)", "text"), ""]
            hunks = relevant_hunks(r["sha"], r["filename"], r["orig"])
            if hunks:
                out += ["**Diff:**", "", fence("\n".join(hunks), "diff"), ""]
            later = later_ai_edits(path, r, later_mode)
            if later:
                out += [f"**Later AI edits ({later_mode}):**", ""]
                for e in later:
                    out += [f"- `{e['short']}` by `{e['author']}` ({e['date'][:16].replace('T', ' ')}): {e['subject']}", "",
                            fence(e["prompt"] or "(no prompt found)", "text"), ""]
    if not ranges_by_file:
        out += ["_No surviving lines found for this model in the selected period._", ""]
    return "\n".join(out)


# --------------------------------------------------------------------------
# input handling (flags first, interactive menu for anything missing)
# --------------------------------------------------------------------------
def ask_choice(title, options):
    print(f"\n{title}")
    for i, o in enumerate(options, 1):
        print(f"  {i}) {o}")
    while True:
        s = input("> ").strip()
        if s.isdigit() and 1 <= int(s) <= len(options):
            return int(s) - 1
        print(f"Enter a number 1-{len(options)}.")


def parse_date(s, end_of_day=False):
    d = datetime.strptime(s, "%Y-%m-%d")
    return d + timedelta(days=1) - timedelta(seconds=1) if end_of_day else d


def ask_date(prompt, end_of_day=False, optional=False):
    while True:
        s = input(prompt).strip()
        if not s and optional:
            return None
        try:
            return parse_date(s, end_of_day)
        except ValueError:
            print("Use format YYYY-MM-DD.")


def choose_model(models):
    names = sorted(models)
    labels = [f"{n}  ({models[n]} commits)" for n in names]
    return names[ask_choice("Which model?", labels)]


def choose_period():
    today = datetime.now().replace(hour=0, minute=0, second=0, microsecond=0)
    i = ask_choice("Time span?", ["Today", "Last 7 days", "Last 30 days", "All time", "Custom dates"])
    if i == 0:
        return today, None
    if i == 1:
        return today - timedelta(days=7), None
    if i == 2:
        return today - timedelta(days=30), None
    if i == 3:
        return None, None
    return (ask_date("From (YYYY-MM-DD): "),
            ask_date("To (YYYY-MM-DD, empty = now): ", end_of_day=True, optional=True))


def main():
    ap = argparse.ArgumentParser(description="Find lines in HEAD written by a specific AI model.")
    ap.add_argument("--model", help="exact model version (git author name)")
    ap.add_argument("--since", help="start date YYYY-MM-DD (commit author date)")
    ap.add_argument("--until", help="end date YYYY-MM-DD (inclusive)")
    ap.add_argument("--all", action="store_true", help="no time limit")
    ap.add_argument("--out", choices=["terminal", "file"], help="where to write the report")
    ap.add_argument("--file", help="output filename (default: ai-report-<model>-<date>.md)")
    ap.add_argument("--later", choices=["lines", "file"], default="lines",
                    help="later AI edits: to the same lines (default) or anywhere in the file")
    args = ap.parse_args()

    root = repo_root()
    import os
    os.chdir(root)

    models = list_models()
    if not models:
        sys.exit("No AI-authored commits found in this repository.")

    model = args.model
    if not model:
        model = choose_model(models)
    elif model not in models:
        print(f"Model '{model}' not found. Known models: {', '.join(sorted(models))}")
        sys.exit(1)

    if args.all:
        start = end = None
    elif args.since or args.until:
        try:
            start = parse_date(args.since) if args.since else None
            end = parse_date(args.until, True) if args.until else None
        except ValueError:
            sys.exit("Dates must be in format YYYY-MM-DD.")
    else:
        start, end = choose_period()

    out_mode = args.out
    if not out_mode:
        out_mode = ["terminal", "file"][ask_choice("Output?", ["Terminal", "File (markdown)"])]

    period = f"{start:%Y-%m-%d} to {end:%Y-%m-%d}" if start and end else \
             f"since {start:%Y-%m-%d}" if start else \
             f"until {end:%Y-%m-%d}" if end else "all time"

    print("\nAnalysing git history...", file=sys.stderr)
    files = candidate_files(model)
    ranges = find_ranges(model, files, start, end)
    report = build_report(model, period, ranges, args.later)

    if out_mode == "terminal":
        print(report)
    else:
        name = args.file or f"ai-report-{model}-{datetime.now():%Y-%m-%d}.md"
        with open(name, "w", encoding="utf-8") as f:
            f.write(report)
        print(f"Report written to {name}")


if __name__ == "__main__":
    main()