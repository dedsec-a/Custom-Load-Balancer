"""
Sweep backend settings (cores x DB pool), load-test each combination with
`hey`, and plot how latency changes.

Run from anywhere:
    python scripts/sweep.py
    python scripts/sweep.py --endpoint search -n 400 -c 50

Needs:  pip install pandas seaborn matplotlib
        hey on your PATH (go install github.com/rakyll/hey@latest)

Writes: results/sweep_<endpoint>.csv and results/sweep_<endpoint>.png
"""

import argparse
import io
import os
import shutil
import subprocess
import sys
import time
import urllib.request
from pathlib import Path

import matplotlib.pyplot as plt
import pandas as pd
import seaborn as sns

# ---- What to sweep -------------------------------------------------------
CORES = [1, 2, 4, 8]
DB_POOLS = [1, 2, 5, 10, 20, 50]
BASE_PORT = 9100  # each run gets its own port: 9100, 9101, ...

ROOT = Path(__file__).resolve().parent.parent  # project root (where go.mod is)
EXE = ".exe" if os.name == "nt" else ""
BACKEND = ROOT / "bin" / f"backend{EXE}"
RESULTS = ROOT / "results"


def find_hey() -> str:
    """Find hey on PATH, or in Go's install folder (~/go/bin)."""
    path = shutil.which("hey") or str(Path.home() / "go" / "bin" / f"hey{EXE}")
    if not Path(path).exists():
        sys.exit("Can't find hey. Install it: go install github.com/rakyll/hey@latest")
    return path


def build_backend() -> None:
    """Compile the backend once, so every run uses the latest code."""
    print("Building backend...")
    subprocess.run(
        ["go", "build", "-o", str(BACKEND), "./cmd/backend"], cwd=ROOT, check=True
    )


def wait_until_up(port: int, timeout: float = 5.0) -> None:
    """Poll /health until the server answers, instead of guessing a sleep time."""
    deadline = time.time() + timeout
    while time.time() < deadline:
        try:
            urllib.request.urlopen(f"http://127.0.0.1:{port}/health", timeout=0.5)
            return
        except OSError:
            time.sleep(0.1)
    raise RuntimeError(f"backend on port {port} did not start")


def run_one(hey: str, cores: int, db_pool: int, port: int, args) -> dict:
    """Start one backend, load-test it with hey, stop it, return summary stats."""
    server = subprocess.Popen(
        [str(BACKEND), "--port", str(port), "--name", f"c{cores}-db{db_pool}",
         "--cores", str(cores), "--db-pool", str(db_pool)],
        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
    )
    try:
        wait_until_up(port)
        url = f"http://127.0.0.1:{port}/{args.endpoint}"
        # -o csv: one row per request instead of the human-readable summary
        out = subprocess.run(
            [hey, "-n", str(args.n), "-c", str(args.c), "-o", "csv", url],
            capture_output=True, text=True, check=True,
        ).stdout
    finally:
        server.terminate()
        server.wait()

    reqs = pd.read_csv(io.StringIO(out))
    ok = reqs[reqs["status-code"] == 200]
    ms = ok["response-time"] * 1000  # hey reports seconds
    # total test duration = when the last request finished
    duration = (reqs["offset"] + reqs["response-time"]).max()

    return {
        "cores": cores,
        "db_pool": db_pool,
        "p50_ms": ms.quantile(0.50),
        "p95_ms": ms.quantile(0.95),
        "avg_ms": ms.mean(),
        "req_per_sec": len(reqs) / duration,
        "errors": len(reqs) - len(ok),
    }


def plot(df: pd.DataFrame, args) -> Path:
    sns.set_theme(style="whitegrid")
    fig, (left, right) = plt.subplots(1, 2, figsize=(14, 5.5))

    # Heatmap: rows = DB pool (big at top), columns = cores, colour = p95
    grid = df.pivot(index="db_pool", columns="cores", values="p95_ms").sort_index(
        ascending=False
    )
    sns.heatmap(grid, annot=True, fmt=".0f", cmap="rocket_r", ax=left,
                cbar_kws={"label": "p95 latency (ms)"})
    left.set_title("p95 latency by cores and DB pool")
    left.set_xlabel("CPU cores")
    left.set_ylabel("DB pool size")

    # Lines: one per core count, showing where adding DB connections stops helping
    sns.lineplot(data=df, x="db_pool", y="p95_ms", hue="cores", marker="o",
                 palette="viridis", ax=right)
    right.set_xscale("log")
    right.set_xticks(DB_POOLS, labels=[str(p) for p in DB_POOLS])
    right.set_title("Adding DB connections: where does it stop helping?")
    right.set_xlabel("DB pool size (log scale)")
    right.set_ylabel("p95 latency (ms)")
    right.legend(title="CPU cores")

    fig.suptitle(f"/{args.endpoint}  ·  hey -n {args.n} -c {args.c}", fontsize=13)
    fig.tight_layout()
    path = RESULTS / f"sweep_{args.endpoint}.png"
    fig.savefig(path, dpi=130)
    return path


def main() -> None:
    p = argparse.ArgumentParser(description=__doc__,
                                formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("--endpoint", default="browse", help="browse or search")
    p.add_argument("-n", type=int, default=200, help="total requests per run")
    p.add_argument("-c", type=int, default=50, help="concurrent workers")
    p.add_argument("--no-build", action="store_true", help="skip go build")
    args = p.parse_args()

    hey = find_hey()
    if not args.no_build:
        build_backend()
    RESULTS.mkdir(exist_ok=True)

    rows = []
    total = len(CORES) * len(DB_POOLS)
    for i, (cores, db_pool) in enumerate(
        (c, d) for c in CORES for d in DB_POOLS
    ):
        row = run_one(hey, cores, db_pool, BASE_PORT + i, args)
        rows.append(row)
        print(f"[{i + 1:2}/{total}] cores={cores:<2} db_pool={db_pool:<3} "
              f"p50={row['p50_ms']:7.1f} ms  p95={row['p95_ms']:7.1f} ms  "
              f"{row['req_per_sec']:7.0f} req/s  errors={row['errors']}")

    df = pd.DataFrame(rows)
    csv_path = RESULTS / f"sweep_{args.endpoint}.csv"
    df.to_csv(csv_path, index=False)
    png_path = plot(df, args)
    print(f"\nSaved {csv_path}\nSaved {png_path}")
    plt.show()  # opens a window with the chart; close it to finish


if __name__ == "__main__":
    main()