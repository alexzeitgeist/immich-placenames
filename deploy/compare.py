#!/usr/bin/env python3
"""Compare a dry run with the database's names, by asset id.

usage: compare.py DB.csv DRY-RUN.csv

DB.csv: headerless CSV with columns id,latitude,longitude,city,state,country.
DRY-RUN.csv: the output of run -dry-run. Prints the counts, then every
renaming with the number of assets it touches.
"""
import csv
import hashlib
import sys
from collections import Counter

db_path, dry_path = sys.argv[1], sys.argv[2]
db = {}
with open(db_path, newline="") as f:
    for row in csv.reader(f):
        db[row[0]] = tuple(row[3:6])
dry = {}
with open(dry_path, newline="") as f:
    for line in f:
        if line.startswith("#") or not line.strip():
            continue
        row = next(csv.reader([line]))
        if len(row) == 4 and row[0] in db:
            dry[row[0]] = tuple(row[1:4])
different = {k: v for k, v in dry.items() if db[k] != v}
print(f"Database assets: {len(db)}")
print(f"Resolved: {len(dry)}")
print(f"Equal: {len(dry) - len(different)}")
print(f"Different: {len(different)}")
print("Database CSV SHA256:", hashlib.sha256(open(db_path, "rb").read()).hexdigest())
if different:
    fields = Counter()
    pairs = Counter()
    for k, v in different.items():
        for i, name in enumerate(("city", "state", "country")):
            if db[k][i] != v[i]:
                fields[name] += 1
                pairs[(name, db[k][i], v[i])] += 1
    print("Fields changed:", " ".join(f"{k}={fields[k]}" for k in ("city", "state", "country")))
    print("Renamings, assets:")
    for (name, a, b), n in sorted(pairs.items(), key=lambda x: (-x[1], x[0])):
        print(f"  {name:8s} {n:4d}  {a} -> {b}")
