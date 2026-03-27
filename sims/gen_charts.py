#!/usr/bin/env python3
"""Generate comparison charts for sim runs 7, 8, 9 (Tool-a-thon suite)."""

import sqlite3
import matplotlib.pyplot as plt
import matplotlib as mpl
import numpy as np
from pathlib import Path
from datetime import datetime

# --- Config ---
DB_PATH = Path(__file__).parent / "sim.db"
OUT_DIR = Path(__file__).parent / "results"
OUT_DIR.mkdir(exist_ok=True)

RUNS = {
    7: ("Trinity", "#FFD700"),        # gold
    8: ("DeepSeek V3.2", "#4ADE80"),  # green
    9: ("Mercury 2", "#22D3EE"),      # cyan
}
RUN_IDS = [7, 8, 9]

AGENT_MODELS = {
    7: "arcee-ai/trinity-large-preview:free",
    8: "deepseek/deepseek-v3.2-20251201",
    9: "inception/mercury-2-20260304",
}

# Dark theme
mpl.rcParams.update({
    "figure.facecolor": "#0d0d0d",
    "axes.facecolor": "#161616",
    "axes.edgecolor": "#2a2a2a",
    "axes.labelcolor": "#bbb",
    "xtick.color": "#999",
    "ytick.color": "#999",
    "text.color": "#ddd",
    "grid.color": "#222",
    "grid.alpha": 0.5,
    "font.family": "sans-serif",
    "font.size": 11,
    "legend.facecolor": "#1a1a1a",
    "legend.edgecolor": "#333",
    "figure.dpi": 150,
})

TURN_RANGE = range(1, 14)

conn = sqlite3.connect(str(DB_PATH))
conn.row_factory = sqlite3.Row


# ============================================================
# Helpers
# ============================================================

def get_per_turn_duration(run_id):
    """Wall-clock seconds per turn from message timestamps (user -> assistant pairs)."""
    rows = conn.execute(
        "SELECT turn_number, role, timestamp FROM sim_messages WHERE run_id=? ORDER BY id",
        (run_id,),
    ).fetchall()
    durations = {}
    for i in range(0, len(rows) - 1, 2):
        if rows[i]["role"] == "user" and rows[i + 1]["role"] == "assistant":
            turn_num = i // 2 + 1
            t0 = datetime.fromisoformat(rows[i]["timestamp"].replace("Z", "+00:00"))
            t1 = datetime.fromisoformat(rows[i + 1]["timestamp"].replace("Z", "+00:00"))
            durations[turn_num] = (t1 - t0).total_seconds()
    return durations


def get_per_turn_cost(run_id):
    """Assign metrics to turns by matching timestamps against agent_turns boundaries."""
    turn_bounds = conn.execute(
        "SELECT turn_number, MIN(timestamp) as t_start, MAX(timestamp) as t_end "
        "FROM sim_agent_turns WHERE run_id=? GROUP BY turn_number ORDER BY turn_number",
        (run_id,),
    ).fetchall()
    metrics = conn.execute(
        "SELECT cost_usd, timestamp FROM sim_metrics WHERE run_id=? ORDER BY timestamp",
        (run_id,),
    ).fetchall()
    per_turn = {}
    for tb in turn_bounds:
        tn = tb["turn_number"]
        cost = sum(
            m["cost_usd"] for m in metrics
            if tb["t_start"] <= m["timestamp"] <= tb["t_end"]
        )
        per_turn[tn] = cost
    return per_turn


def bar_label(ax, bars, fmt="{:.2f}", offset=0.02, **kwargs):
    """Add value labels on top of bars."""
    y_max = ax.get_ylim()[1]
    for bar in bars:
        h = bar.get_height() + bar.get_y() if hasattr(bar, "get_y") else bar.get_height()
        ax.text(
            bar.get_x() + bar.get_width() / 2,
            h + y_max * offset,
            fmt.format(h),
            ha="center", va="bottom", fontweight="bold", fontsize=10,
            **kwargs,
        )


# ============================================================
# Chart 1: Overall Comparison
# ============================================================

def chart_overall():
    fig, axes = plt.subplots(1, 3, figsize=(15, 5.5))
    fig.suptitle("Overall Comparison \u2014 Tool-a-thon Suite", fontsize=17, fontweight="bold", y=1.0)

    names = [RUNS[r][0] for r in RUN_IDS]
    colors = [RUNS[r][1] for r in RUN_IDS]

    # --- Total Cost ---
    costs = []
    for rid in RUN_IDS:
        row = conn.execute("SELECT total_cost_usd FROM sim_runs WHERE id=?", (rid,)).fetchone()
        costs.append(row["total_cost_usd"])

    ax = axes[0]
    bars = ax.bar(names, costs, color=colors, width=0.55, edgecolor="#444", linewidth=0.5)
    ax.set_title("Total Cost", fontsize=13, fontweight="bold")
    ax.set_ylabel("USD")
    for bar, v in zip(bars, costs):
        ax.text(bar.get_x() + bar.get_width() / 2, bar.get_height() + max(costs) * 0.03,
                f"${v:.3f}", ha="center", va="bottom", fontsize=11, fontweight="bold")
    ax.set_ylim(0, max(costs) * 1.25)
    ax.grid(axis="y", alpha=0.3)
    ax.set_axisbelow(True)

    # --- Duration (adjusted) ---
    durations = []
    for rid in RUN_IDS:
        row = conn.execute("SELECT duration_ms FROM sim_runs WHERE id=?", (rid,)).fetchone()
        d = row["duration_ms"] / 1000
        if rid == 7:
            d -= 60  # subtract 5s x 12 artificial delays
        durations.append(d)

    ax = axes[1]
    bars = ax.bar(names, durations, color=colors, width=0.55, edgecolor="#444", linewidth=0.5)
    ax.set_title("Wall-Clock Duration\n(Trinity -60s for delays)", fontsize=13, fontweight="bold")
    ax.set_ylabel("Seconds")
    for bar, v in zip(bars, durations):
        m, s = divmod(int(v), 60)
        lbl = f"{m}m {s}s" if m else f"{s}s"
        ax.text(bar.get_x() + bar.get_width() / 2, bar.get_height() + max(durations) * 0.03,
                lbl, ha="center", va="bottom", fontsize=11, fontweight="bold")
    ax.set_ylim(0, max(durations) * 1.25)
    ax.grid(axis="y", alpha=0.3)
    ax.set_axisbelow(True)

    # --- Agent Tokens ---
    tokens = []
    for rid in RUN_IDS:
        row = conn.execute(
            "SELECT SUM(total_tokens) as t FROM sim_metrics WHERE run_id=? AND model=?",
            (rid, AGENT_MODELS[rid]),
        ).fetchone()
        tokens.append(row["t"] or 0)

    ax = axes[2]
    bars = ax.bar(names, [t / 1000 for t in tokens], color=colors, width=0.55, edgecolor="#444", linewidth=0.5)
    ax.set_title("Agent Model Tokens", fontsize=13, fontweight="bold")
    ax.set_ylabel("Thousands")
    for bar, v in zip(bars, tokens):
        ax.text(bar.get_x() + bar.get_width() / 2, bar.get_height() / 1000 + max(tokens) / 1000 * 0.03,
                f"{v / 1000:.0f}K", ha="center", va="bottom", fontsize=11, fontweight="bold")
    ax.set_ylim(0, max(tokens) / 1000 * 1.25)
    ax.grid(axis="y", alpha=0.3)
    ax.set_axisbelow(True)

    fig.tight_layout()
    fig.savefig(OUT_DIR / "01_overall_comparison.png", bbox_inches="tight", facecolor=fig.get_facecolor())
    plt.close(fig)
    print("Saved 01_overall_comparison.png")


# ============================================================
# Chart 2: Per-Turn Cost Breakdown
# ============================================================

def chart_per_turn_cost():
    fig, ax = plt.subplots(figsize=(14, 6))
    fig.suptitle("Per-Turn Cost Breakdown", fontsize=17, fontweight="bold")

    x = np.arange(len(list(TURN_RANGE)))
    width = 0.25

    for i, rid in enumerate(RUN_IDS):
        name, color = RUNS[rid]
        pt_cost = get_per_turn_cost(rid)
        vals = [pt_cost.get(t, 0) * 100 for t in TURN_RANGE]  # cents
        offset = (i - 1) * width
        ax.bar(x + offset, vals, width, label=name, color=color, edgecolor="#444", linewidth=0.4)

    ax.set_xlabel("Turn Number", fontsize=12)
    ax.set_ylabel("Cost (cents)", fontsize=12)
    ax.set_title("Where each model spends the most", fontsize=11, color="#888")
    ax.set_xticks(x)
    ax.set_xticklabels(list(TURN_RANGE))
    ax.legend(fontsize=11)
    ax.grid(axis="y", alpha=0.3)
    ax.set_axisbelow(True)

    fig.tight_layout()
    fig.savefig(OUT_DIR / "02_per_turn_cost.png", bbox_inches="tight", facecolor=fig.get_facecolor())
    plt.close(fig)
    print("Saved 02_per_turn_cost.png")


# ============================================================
# Chart 3: Per-Turn Latency
# ============================================================

def chart_per_turn_latency():
    fig, ax = plt.subplots(figsize=(14, 6))
    fig.suptitle("Per-Turn Wall-Clock Latency", fontsize=17, fontweight="bold")

    x = np.arange(len(list(TURN_RANGE)))
    width = 0.25

    for i, rid in enumerate(RUN_IDS):
        name, color = RUNS[rid]
        durations = get_per_turn_duration(rid)
        vals = []
        for t in TURN_RANGE:
            d = durations.get(t, 0)
            if rid == 7 and d > 0:
                d = max(0, d - 5)  # subtract 5s artificial delay per turn
            vals.append(d)
        offset = (i - 1) * width
        ax.bar(x + offset, vals, width, label=name, color=color, edgecolor="#444", linewidth=0.4)

    ax.set_xlabel("Turn Number", fontsize=12)
    ax.set_ylabel("Duration (seconds)", fontsize=12)
    ax.set_title("Trinity adjusted -5s/turn for artificial inter-turn delay", fontsize=11, color="#888")
    ax.set_xticks(x)
    ax.set_xticklabels(list(TURN_RANGE))
    ax.legend(fontsize=11)
    ax.grid(axis="y", alpha=0.3)
    ax.set_axisbelow(True)

    fig.tight_layout()
    fig.savefig(OUT_DIR / "03_per_turn_latency.png", bbox_inches="tight", facecolor=fig.get_facecolor())
    plt.close(fig)
    print("Saved 03_per_turn_latency.png")


# ============================================================
# Chart 4: Token Efficiency (Stacked: prompt vs completion)
# ============================================================

def chart_token_efficiency():
    fig, ax = plt.subplots(figsize=(11, 6))
    fig.suptitle("Token Efficiency \u2014 Agent Model Only", fontsize=17, fontweight="bold")

    names = []
    prompt_k = []
    completion_k = []
    colors_list = []

    for rid in RUN_IDS:
        name, color = RUNS[rid]
        row = conn.execute(
            "SELECT SUM(prompt_tokens) as pt, SUM(completion_tokens) as ct "
            "FROM sim_metrics WHERE run_id=? AND model=?",
            (rid, AGENT_MODELS[rid]),
        ).fetchone()
        names.append(name)
        prompt_k.append((row["pt"] or 0) / 1000)
        completion_k.append((row["ct"] or 0) / 1000)
        colors_list.append(color)

    x = np.arange(len(names))
    width = 0.45

    # Prompt tokens (bottom, dimmer)
    ax.bar(x, prompt_k, width, label="Prompt Tokens",
           color=[c + "66" for c in colors_list], edgecolor="#444", linewidth=0.5)
    # Completion tokens (top, bright)
    ax.bar(x, completion_k, width, bottom=prompt_k, label="Completion Tokens",
           color=colors_list, edgecolor="#444", linewidth=0.5)

    # Ratio labels
    top_val = max(p + c for p, c in zip(prompt_k, completion_k))
    for i in range(len(names)):
        total = prompt_k[i] + completion_k[i]
        ratio = prompt_k[i] / completion_k[i] if completion_k[i] > 0 else 0
        ax.text(x[i], total + top_val * 0.03,
                f"{ratio:.0f}:1 prompt:completion\n{total:.0f}K total",
                ha="center", va="bottom", fontsize=10, fontweight="bold")

    ax.set_ylabel("Tokens (thousands)", fontsize=12)
    ax.set_title("Lower prompt:completion ratio = model generates more output per input", fontsize=10, color="#888")
    ax.set_xticks(x)
    ax.set_xticklabels(names, fontsize=12)
    ax.legend(fontsize=11, loc="upper right")
    ax.grid(axis="y", alpha=0.3)
    ax.set_axisbelow(True)
    ax.set_ylim(0, top_val * 1.25)

    fig.tight_layout()
    fig.savefig(OUT_DIR / "04_token_efficiency.png", bbox_inches="tight", facecolor=fig.get_facecolor())
    plt.close(fig)
    print("Saved 04_token_efficiency.png")


# ============================================================
# Chart 5: Agent Loop Depth
# ============================================================

def chart_agent_loop_depth():
    fig, ax = plt.subplots(figsize=(14, 6))
    fig.suptitle("Agent Loop Depth \u2014 Iterations Per Turn", fontsize=17, fontweight="bold")

    x = np.arange(len(list(TURN_RANGE)))
    width = 0.25

    averages = {}
    for i, rid in enumerate(RUN_IDS):
        name, color = RUNS[rid]
        rows = conn.execute(
            "SELECT turn_number, COUNT(*) as cnt FROM sim_agent_turns "
            "WHERE run_id=? AND role='assistant' GROUP BY turn_number",
            (rid,),
        ).fetchall()
        depth_map = {r["turn_number"]: r["cnt"] for r in rows}
        depths = [depth_map.get(t, 0) for t in TURN_RANGE]
        offset = (i - 1) * width
        ax.bar(x + offset, depths, width, label=name, color=color, edgecolor="#444", linewidth=0.4)

        # Average
        non_zero = [d for d in depths if d > 0]
        avg = sum(non_zero) / len(non_zero) if non_zero else 0
        averages[rid] = avg

    # Average lines
    for rid in RUN_IDS:
        name, color = RUNS[rid]
        ax.axhline(y=averages[rid], color=color, linestyle="--", alpha=0.5, linewidth=1.2)
        ax.text(len(list(TURN_RANGE)) - 0.3, averages[rid] + 0.2,
                f"{name} avg: {averages[rid]:.1f}",
                color=color, fontsize=9, ha="right", fontweight="bold")

    ax.set_xlabel("Turn Number", fontsize=12)
    ax.set_ylabel("Agent Iterations (assistant actions)", fontsize=12)
    ax.set_title("Fewer iterations = more decisive model", fontsize=11, color="#888")
    ax.set_xticks(x)
    ax.set_xticklabels(list(TURN_RANGE))
    ax.legend(fontsize=11, loc="upper left")
    ax.grid(axis="y", alpha=0.3)
    ax.set_axisbelow(True)

    fig.tight_layout()
    fig.savefig(OUT_DIR / "05_agent_loop_depth.png", bbox_inches="tight", facecolor=fig.get_facecolor())
    plt.close(fig)
    print("Saved 05_agent_loop_depth.png")


# ============================================================
# Chart 6: Fact Quality Comparison
# ============================================================

def chart_fact_quality():
    fig = plt.figure(figsize=(18, 11))
    fig.suptitle("Fact Quality Comparison", fontsize=17, fontweight="bold", y=0.97)

    ax = fig.add_subplot(111)
    ax.axis("off")

    col_positions = [0.01, 0.34, 0.67]
    col_width = 0.30
    y_start = 0.90

    for col_idx, rid in enumerate(RUN_IDS):
        name, color = RUNS[rid]
        facts = conn.execute(
            "SELECT fact, category, importance FROM sim_facts WHERE run_id=? ORDER BY importance DESC",
            (rid,),
        ).fetchall()

        x_pos = col_positions[col_idx]

        # Header
        ax.text(x_pos + col_width / 2, y_start, name,
                fontsize=15, fontweight="bold", color=color,
                ha="center", va="top", transform=ax.transAxes)
        ax.text(x_pos + col_width / 2, y_start - 0.04,
                f"{len(facts)} facts extracted",
                fontsize=10, color="#777",
                ha="center", va="top", transform=ax.transAxes)

        y = y_start - 0.10
        for fact in facts:
            imp = fact["importance"]
            cat = fact["category"]
            text = fact["fact"]

            # Importance color
            if imp >= 9:
                imp_color = "#ef4444"
            elif imp >= 7:
                imp_color = "#f59e0b"
            else:
                imp_color = "#6b7280"

            # Badge + category
            ax.text(x_pos, y, f"[{imp}]", fontsize=9, fontweight="bold", color=imp_color,
                    va="top", transform=ax.transAxes, fontfamily="monospace")
            ax.text(x_pos + 0.035, y, f"({cat})", fontsize=8, color="#555",
                    va="top", transform=ax.transAxes)
            y -= 0.035

            # Word-wrap the fact text
            max_chars = 52
            words = text.split()
            lines = []
            line = ""
            for w in words:
                if len(line) + len(w) + 1 > max_chars:
                    lines.append(line)
                    line = w
                else:
                    line = f"{line} {w}" if line else w
            if line:
                lines.append(line)

            for ln in lines:
                ax.text(x_pos + 0.008, y, ln, fontsize=9, color="#ccc",
                        va="top", transform=ax.transAxes)
                y -= 0.028
            y -= 0.018  # gap between facts

    # Vertical separators
    for sep_x in [0.325, 0.655]:
        ax.plot([sep_x, sep_x], [0.02, 0.93], color="#333", linewidth=1,
                transform=ax.transAxes, clip_on=False)

    fig.savefig(OUT_DIR / "06_fact_quality.png", bbox_inches="tight", facecolor=fig.get_facecolor())
    plt.close(fig)
    print("Saved 06_fact_quality.png")


# ============================================================
# Main
# ============================================================

chart_overall()
chart_per_turn_cost()
chart_per_turn_latency()
chart_token_efficiency()
chart_agent_loop_depth()
chart_fact_quality()

conn.close()
print(f"\nAll charts saved to {OUT_DIR}/")
