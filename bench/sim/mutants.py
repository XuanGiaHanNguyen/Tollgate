#!/usr/bin/env python3
"""Mutation check for the Raft simulator: plant known Raft bugs one at a
time and confirm the deterministic simulation catches each. Restores the
source afterwards. A mutant that fails to compile is reported as such,
never as "not caught".

    python3 bench/sim/mutants.py [seeds]
"""
import shutil, subprocess, sys, tempfile

SRC = "internal/raft/raft.go"
SEEDS = sys.argv[1] if len(sys.argv) > 1 else "3000"

MUTANTS = {
    "vote ignores log up-to-date check": [(
        "\t\tgrant := (n.vote == 0 || n.vote == m.From) && upToDate && n.state != Leader",
        "\t\t_ = upToDate\n\t\tgrant := (n.vote == 0 || n.vote == m.From) && n.state != Leader")],
    "vote and pre-vote ignore log up-to-date check": [
        ("\t\tgrant := (n.vote == 0 || n.vote == m.From) && upToDate && n.state != Leader",
         "\t\t_ = upToDate\n\t\tgrant := (n.vote == 0 || n.vote == m.From) && n.state != Leader"),
        ("\t\tif m.Term > n.term && upToDate && !leaderAlive {",
         "\t\t_ = upToDate\n\t\tif m.Term > n.term && !leaderAlive {")],
    "commit earlier-term entries by count (Figure 8)": [(
        "\tif idx > n.committed && n.termAt(idx) == n.term {", "\tif idx > n.committed {")],
    "no step-down on higher term": [(
        "\tcase m.Term > n.term:\n\t\tlead := uint64(0)",
        "\tcase m.Term > n.term && m.Type == MsgVote:\n\t\tlead := uint64(0)")],
    "vote granted twice in a term": [(
        "\t\tgrant := (n.vote == 0 || n.vote == m.From) && upToDate && n.state != Leader",
        "\t\tgrant := upToDate && n.state != Leader")],
}

def main():
    backup = tempfile.mktemp()
    shutil.copy(SRC, backup)
    failed = False
    try:
        orig = open(SRC).read()
        for name, edits in MUTANTS.items():
            s = orig
            for a, b in edits:
                if a not in s:
                    print(f"{name}: PATTERN NOT FOUND (source changed?)"); failed = True; break
                s = s.replace(a, b, 1)
            else:
                open(SRC, "w").write(s)
                vet = subprocess.run(["go", "vet", "./internal/raft"], capture_output=True, text=True)
                if vet.returncode != 0:
                    print(f"{name}: BUILD FAILED\n{vet.stderr[:400]}"); failed = True; continue
                r = subprocess.run(["go", "test", "-count=1", "-run", "TestSeeds$", "./internal/sim", "-args", "-seeds", SEEDS],
                                   capture_output=True, text=True)
                out = r.stdout + r.stderr
                hit = [l.strip() for l in out.splitlines() if "seed " in l and " tick " in l]
                if hit:
                    print(f"{name}: caught -> {hit[0][:150]}")
                else:
                    print(f"{name}: NOT CAUGHT in {SEEDS} seeds"); failed = True
    finally:
        shutil.copy(backup, SRC)
    sys.exit(1 if failed else 0)

if __name__ == "__main__":
    main()
