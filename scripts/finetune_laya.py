#!/usr/bin/env python3
"""Resumable smoke-test fine-tune of laya on the captured classifier corpus.

Five idempotent stages, state kept in the --out directory:

    export      corpus JSONL -> dataset.jsonl (via scripts/corpus_to_laya.py)
    preprocess  dataset.jsonl -> items.pt (laya token sequences + one-hot targets)
    train       RLCD loop ported from laya's Kaggle notebook, rolling checkpoint
                after every epoch; a re-run resumes at the next epoch
    evaluate    base vs fine-tuned accuracy on the held-out slice
    finalize    fit calibration temperatures, write the final checkpoint dir

Re-running the same command over unchanged corpus files resumes. A changed
corpus or configuration discards the saved progress and trains from the base
checkpoint; the proxy appends to today's corpus file while it runs, so copy
the files first to resume across runs. --fresh wipes the state. --dry-run runs
only the pure-python export stage and prints the plan, so the pipeline can be
checked on a machine without torch/laya installed; it never discards progress.

Requires laya 0.3.20 (the script imports laya internals that move between
releases): python3 -m pip install "laya==0.3.20" torch transformers safetensors

Usage:
    python3 scripts/finetune_laya.py ~/.config/antigravity-proxy/corpus/*.jsonl \
        --source gateway --source upstream
"""
import argparse
import contextlib
import hashlib
import json
import math
import random
import shutil
import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

from corpus_to_laya import (  # noqa: E402
    DEFAULT_KINDS,
    DEFAULT_SOURCES,
    KNOWN_KINDS,
    KNOWN_SOURCES,
    convert,
    load_rows,
)

LAYA_VERSION = "0.3.20"
BASE_MODEL_ID = "convaiinnovations/laya"
# The files a laya checkpoint loads from; laya.Agent downloads exactly these.
BASE_MODEL_FILES = ("rl_agent_config.json", "model.safetensors", "tokenizer/*", "encoder/*")
QUESTION_NAME = "risk"
HELD_OUT_FRACTION = 0.10

# Training hyperparameters, identical to notebooks/laya_finetune_typed_decisions_2xT4_kaggle.ipynb
# except GRAD_ACCUM (1 here, 4 there) — a smoke run does not need the large effective batch.
MICRO_BATCH = 8
GRAD_ACCUM = 1
GROUP_SIZE = 4
LR_ENCODER = 2.5e-5
LR_HEAD = 1.0e-4
SIGMA_START = 0.4
SIGMA_END = 0.1
WEIGHT_DECAY = 0.01

STAGES = ("export", "preprocess", "train", "evaluate", "finalize")

INSTALL_HINT = (
    f"missing a dependency; install with: "
    f"python3 -m pip install \"laya=={LAYA_VERSION}\" torch transformers safetensors"
)


def require_heavy():
    """Import torch/transformers/laya lazily so --dry-run and tests need neither."""
    try:
        import numpy  # noqa: F401
        import torch
        from transformers import AutoTokenizer  # noqa: F401
        from laya import common  # noqa: F401
    except ImportError as exc:
        raise SystemExit(f"{exc.name}: {INSTALL_HINT}")
    import laya

    if laya.__version__ != LAYA_VERSION:
        print(
            f"WARNING: laya {laya.__version__} installed, this script targets {LAYA_VERSION}; "
            "the internals it imports move between releases.",
            file=sys.stderr,
        )
    return torch


# ---------------------------------------------------------------------------
# Stage state: <out>/state.json records the corpus hash, the config signature
# and how far training got. Every stage consults it before doing work.
# ---------------------------------------------------------------------------


def corpus_sha256(paths):
    """Hash the corpus files' resolved paths and contents.

    The digest decides whether saved progress is still valid, so a relative
    and an absolute spelling of one file must hash the same.
    """
    digest = hashlib.sha256()
    for path in sorted(Path(p).resolve() for p in paths):
        digest.update(str(path).encode())
        digest.update(path.read_bytes())
    return digest.hexdigest()


def config_signature(args):
    return {
        "base_model": args.base_model,
        "epochs": args.epochs,
        "kinds": sorted(args.kind or DEFAULT_KINDS),
        "max_items": args.max_items,
        "seed": args.seed,
        "sources": sorted(args.source or DEFAULT_SOURCES),
    }


def load_state(out_dir):
    path = out_dir / "state.json"
    if not path.exists():
        return {}
    return json.loads(path.read_text())


def save_state(out_dir, state):
    (out_dir / "state.json").write_text(json.dumps(state, indent=2) + "\n")


# Everything derived from dataset.jsonl. A changed corpus or configuration
# makes all of it stale, including a complete staging checkpoint that the next
# run's recover_checkpoint would otherwise promote.
PROGRESS_FILES = ("items.pt", "eval.json")
PROGRESS_DIRS = ("checkpoint_latest", "checkpoint_latest.staging", "checkpoint_latest.old", "model")


def discard_progress(out_dir):
    for name in PROGRESS_FILES:
        (out_dir / name).unlink(missing_ok=True)
    for name in PROGRESS_DIRS:
        shutil.rmtree(out_dir / name, ignore_errors=True)


def one_hot_target(label, keys):
    """Map a hard bucket label to a target distribution over the criteria keys.

    The exporter emits one label per row ("B"); laya's training loop consumes a
    probability distribution. One-hot is the faithful mapping; the notebook
    normalizes targets, so no smoothing is needed here.
    """
    if label not in keys:
        raise ValueError(f"answer label {label!r} is not one of the criteria {keys}")
    return [1.0 if key == label else 0.0 for key in keys]


# ---------------------------------------------------------------------------
# Stage 1: export (pure python — no torch, no laya)
# ---------------------------------------------------------------------------


def stage_export(args, out_dir, state, digest, write=True):
    """Return the exported examples, reusing dataset.jsonl for an unchanged corpus.

    write=False computes the export without touching the run directory.
    """
    dataset = out_dir / "dataset.jsonl"
    if state.get("corpus_sha256") == digest and dataset.exists():
        print("export: corpus unchanged, keeping dataset.jsonl")
        return [json.loads(line) for line in dataset.read_text().splitlines() if line.strip()]
    rows = []
    unparseable = 0
    for path in args.corpus:
        file_rows, file_skipped = load_rows(path)
        rows.extend(file_rows)
        unparseable += file_skipped
    kinds = tuple(args.kind) if args.kind else DEFAULT_KINDS
    sources = tuple(args.source) if args.source else DEFAULT_SOURCES
    examples, stats = convert(rows, kinds, sources)
    if args.max_items:
        examples = examples[: args.max_items]
    if not examples:
        raise SystemExit(
            "no labelled rows survived the export filters; pass --source/--kind to widen them "
            f"(skipped: {stats['skipped_source']} source, {stats['skipped_kind']} kind, "
            f"{stats['skipped_unlabelled']} unlabelled, {stats['skipped_no_action']} no action)"
        )
    if write:
        with open(dataset, "w", encoding="utf-8") as handle:
            for example in examples:
                handle.write(json.dumps(example, ensure_ascii=False) + "\n")
    print(
        f"export: {'wrote' if write else 'would write'} {len(examples)} examples "
        f"({stats['recovered']} recovered from unclosed tags, {unparseable} unparseable lines)"
    )
    if len(stats["models"]) > 1:
        print(
            f"WARNING: {len(stats['models'])} distinct models graded these rows "
            f"({', '.join(stats['models'])}); each is a different teacher.",
            file=sys.stderr,
        )
    return examples


# ---------------------------------------------------------------------------
# Stage 2: preprocess — token sequences in the notebook's item shape
# ---------------------------------------------------------------------------


def build_items(examples, tok, cfg):
    from laya.common import QTYPES, build_sequence, render_options

    items = []
    skipped = 0
    for example in examples:
        question = example["questions"][QUESTION_NAME]
        criteria = question["criteria"]
        keys = list(criteria.keys())
        target = one_hot_target(example["answers"][QUESTION_NAME], keys)
        seq, markers = build_sequence(
            tok,
            example["state"],
            {"t": question["type"], "ins": question["instructions"], "crit": criteria},
            cfg["max_len"],
            cfg["head_max_len"],
        )
        if len(markers) != len(render_options({"t": question["type"], "crit": criteria})):
            skipped += 1
            continue
        items.append(
            {
                "ids": seq,
                "markers": markers,
                "qtype": QTYPES[question["type"]],
                "target": target,
                "label": target.index(max(target)),
            }
        )
    return items, skipped


def stage_preprocess(args, out_dir, state, examples, digest):
    items_path = out_dir / "items.pt"
    if state.get("items_sha256") == digest and items_path.exists():
        print("preprocess: dataset unchanged, keeping items.pt")
        return
    torch = require_heavy()
    from huggingface_hub import snapshot_download
    from transformers import AutoTokenizer
    from laya.agent import _fix_tokenizer_config

    # The hub repo bundles sibling checkpoints (multilingual/, typed-decisions/);
    # fetch the root one alone, as laya.Agent does.
    model_dir = snapshot_download(args.base_model, allow_patterns=list(BASE_MODEL_FILES))
    _fix_tokenizer_config(model_dir)
    tok = AutoTokenizer.from_pretrained(str(Path(model_dir) / "tokenizer"))
    cfg = json.loads((Path(model_dir) / "rl_agent_config.json").read_text())
    cfg.setdefault("max_len", 512)
    cfg.setdefault("head_max_len", 192)
    (out_dir / "base_config.json").write_text(json.dumps(cfg, indent=2) + "\n")
    (out_dir / "base_model_dir").write_text(model_dir)

    items, skipped = build_items(examples, tok, cfg)
    order = list(range(len(items)))
    random.Random(args.seed).shuffle(order)
    n_held_out = len(items) // int(1 / HELD_OUT_FRACTION)
    held_out = [items[i] for i in sorted(order[:n_held_out])]
    train = [items[i] for i in sorted(order[n_held_out:])]
    # Our own file, written two lines up; weights_only=False keeps plain dicts loadable.
    torch.save({"train": train, "held_out": held_out}, items_path)
    print(f"preprocess: {len(train)} train + {len(held_out)} held-out items ({skipped} skipped)")


# ---------------------------------------------------------------------------
# Stage 3: train — notebook RLCD loop, single device, rolling checkpoint
# ---------------------------------------------------------------------------


def collate_train_batch(items, pad_id, torch):
    n, length = len(items), max(len(it["ids"]) for it in items)
    kmax = max(len(it["markers"]) for it in items)
    ids = torch.full((n, length), pad_id, dtype=torch.long)
    att = torch.zeros((n, length), dtype=torch.long)
    mpos = torch.zeros((n, kmax), dtype=torch.long)
    mmask = torch.zeros((n, kmax), dtype=torch.bool)
    target = torch.zeros((n, kmax), dtype=torch.float32)
    for i, it in enumerate(items):
        ids[i, : len(it["ids"])] = torch.tensor(it["ids"])
        att[i, : len(it["ids"])] = 1
        k = len(it["markers"])
        mpos[i, :k] = torch.tensor(it["markers"])
        mmask[i, :k] = True
        target[i, :k] = torch.tensor(it["target"], dtype=torch.float32)
    return {
        "input_ids": ids,
        "attention_mask": att,
        "marker_pos": mpos,
        "marker_mask": mmask,
        "target": target,
        "qtype": torch.tensor([it["qtype"] for it in items]),
        "label": torch.tensor([it["label"] for it in items]),
    }


def optimizer_steps(n_items, epochs):
    """Optimizer (and scheduler) steps over a run: the partial last batch of an epoch steps too."""
    return math.ceil(n_items / (MICRO_BATCH * GRAD_ACCUM)) * epochs


def resolve_device(requested, torch):
    if requested != "auto":
        return requested
    if torch.cuda.is_available():
        return "cuda"
    if torch.backends.mps.is_available():
        return "mps"
    return "cpu"


def amp_context(device):
    if device == "cuda":
        import torch

        return torch.autocast("cuda", dtype=torch.float16)
    return contextlib.nullcontext()


def make_scaler(device):
    import torch

    try:
        return torch.amp.GradScaler("cuda", enabled=device == "cuda")
    except AttributeError:  # torch < 2.3
        return torch.cuda.amp.GradScaler(enabled=device == "cuda")


def load_model(model_dir, cfg, device, torch, weights_path=None):
    from safetensors.torch import load_file
    from laya.common import build_model

    model = build_model(cfg, encoder_dir=str(Path(model_dir) / "encoder"))
    weights = load_file(weights_path or str(Path(model_dir) / "model.safetensors"))
    model.load_state_dict(weights, strict=True)
    model.head_checkpointing = True
    return model.to(device)


CHECKPOINT_META = "checkpoint_meta.json"


def _staging(ckpt_dir):
    return ckpt_dir.with_name(ckpt_dir.name + ".staging")


def _retired(ckpt_dir):
    return ckpt_dir.with_name(ckpt_dir.name + ".old")


def commit_checkpoint(ckpt_dir):
    """Swap the fully written staging dir in for ckpt_dir.

    Two renames, never an in-place overwrite: a crash at any point leaves the
    previous checkpoint or a complete staging dir that recover_checkpoint
    promotes.
    """
    staging, retired = _staging(ckpt_dir), _retired(ckpt_dir)
    shutil.rmtree(retired, ignore_errors=True)
    if ckpt_dir.exists():
        ckpt_dir.rename(retired)
    staging.rename(ckpt_dir)
    shutil.rmtree(retired, ignore_errors=True)


def recover_checkpoint(ckpt_dir):
    """Finish or roll back a checkpoint swap that a crash interrupted.

    The meta file is written last: a staging dir that has one is complete and
    newer than ckpt_dir, one without it is a torn write.
    """
    if (_staging(ckpt_dir) / CHECKPOINT_META).exists():
        commit_checkpoint(ckpt_dir)
    else:
        shutil.rmtree(_staging(ckpt_dir), ignore_errors=True)
    shutil.rmtree(_retired(ckpt_dir), ignore_errors=True)


def save_checkpoint(ckpt_dir, model, optimizer, scheduler, epoch, avg_loss, tok, torch):
    """Write the rolling checkpoint into a staging dir, meta last, then swap it in.

    Weights keep the model's own dtype: resuming from fp16-rounded weights next
    to fp32 optimizer state would round away an epoch of small updates.
    """
    from safetensors.torch import save_file

    staging = _staging(ckpt_dir)
    shutil.rmtree(staging, ignore_errors=True)
    staging.mkdir(parents=True)
    sd = {k: v.detach().contiguous().cpu() for k, v in model.state_dict().items()}
    save_file(sd, str(staging / "model.safetensors"))
    torch.save(
        {"optimizer": optimizer.state_dict(), "scheduler": scheduler.state_dict()},
        staging / "trainer_state.pt",
    )
    model.encoder.config.save_pretrained(str(staging / "encoder"))
    tok.save_pretrained(str(staging / "tokenizer"))
    (staging / CHECKPOINT_META).write_text(
        json.dumps({"epoch": epoch + 1, "avg_loss": avg_loss}, indent=2) + "\n"
    )
    commit_checkpoint(ckpt_dir)


def stage_train(args, out_dir, state, device):
    torch = require_heavy()
    from transformers import AutoTokenizer
    from laya.common import proper_reward

    cfg = json.loads((out_dir / "base_config.json").read_text())
    model_dir = (out_dir / "base_model_dir").read_text().strip()
    tok = AutoTokenizer.from_pretrained(str(Path(model_dir) / "tokenizer"))
    items = torch.load(out_dir / "items.pt", weights_only=False)
    train_items = items["train"]

    ckpt_dir = out_dir / "checkpoint_latest"
    recover_checkpoint(ckpt_dir)
    meta_path = ckpt_dir / CHECKPOINT_META
    start_epoch = 0
    if meta_path.exists():
        start_epoch = json.loads(meta_path.read_text())["epoch"]
    if start_epoch >= args.epochs:
        print(f"train: checkpoint already at epoch {start_epoch}/{args.epochs}, nothing to do")
        return

    weights = str(ckpt_dir / "model.safetensors") if start_epoch else None
    model = load_model(model_dir, cfg, device, torch, weights_path=weights)
    model.encoder.gradient_checkpointing_enable(gradient_checkpointing_kwargs={"use_reentrant": False})
    model.train()

    enc_params = [p for n, p in model.named_parameters() if "encoder." in n]
    head_params = [p for n, p in model.named_parameters() if "encoder." not in n]
    optimizer = torch.optim.AdamW(
        [{"params": enc_params, "lr": LR_ENCODER}, {"params": head_params, "lr": LR_HEAD}],
        weight_decay=WEIGHT_DECAY,
    )
    total_updates = max(1, optimizer_steps(len(train_items), args.epochs))
    scheduler = torch.optim.lr_scheduler.CosineAnnealingLR(optimizer, T_max=total_updates, eta_min=1e-6)
    scaler = make_scaler(device)

    if start_epoch:
        saved = torch.load(ckpt_dir / "trainer_state.pt", weights_only=False, map_location=device)
        optimizer.load_state_dict(saved["optimizer"])
        scheduler.load_state_dict(saved["scheduler"])
        print(f"train: resumed from epoch {start_epoch}/{args.epochs}")

    t0 = time.time()
    for epoch in range(start_epoch, args.epochs):
        random.seed(args.seed + epoch)
        epoch_items = list(train_items)
        random.shuffle(epoch_items)
        progress = epoch / max(1, args.epochs - 1)
        sigma = SIGMA_START + (SIGMA_END - SIGMA_START) * progress
        epoch_loss, n_batches = 0.0, 0
        optimizer.zero_grad(set_to_none=True)
        accum_step = 0

        for b_idx in range(0, len(epoch_items), MICRO_BATCH):
            chunk = epoch_items[b_idx : b_idx + MICRO_BATCH]
            if not chunk:
                continue
            batch = collate_train_batch(chunk, tok.pad_token_id, torch)
            with amp_context(device):
                logits, act = model(
                    batch["input_ids"].to(device),
                    batch["attention_mask"].to(device),
                    batch["marker_pos"].to(device),
                    batch["marker_mask"].to(device),
                    batch["qtype"].to(device),
                )
            logits = logits.float()
            mask = batch["marker_mask"].to(device)
            k = mask.sum(-1, keepdim=True).float()
            target = batch["target"].to(device)

            # GRPO-style exploration: G noisy logit distributions, zero-mean projected.
            eps = torch.randn((GROUP_SIZE,) + logits.shape, device=device) * sigma * mask
            eps = (eps - eps.sum(-1, keepdim=True) / k) * mask
            z = logits.detach().unsqueeze(0) + eps
            q = torch.softmax(z.masked_fill(~mask, -1e4), -1)

            with torch.no_grad():
                reward = proper_reward(
                    q, target.unsqueeze(0), batch["qtype"].to(device), mask, w_sph=0.75, w_rps=1.0
                )
                adv = reward - reward.mean(0, keepdim=True)
                adv = adv / (adv.std() + 1e-6)

            logp = -(((z - logits.unsqueeze(0)) ** 2) * mask).sum(-1) / (2 * sigma**2)
            loss_rl = -(adv * logp).mean()
            loss_ce = -(target * torch.log_softmax(logits.masked_fill(~mask, -1e4), -1)).sum(-1).mean()
            loss = (loss_rl + loss_ce) / GRAD_ACCUM + 0.0 * act.sum()

            scaler.scale(loss).backward()
            accum_step += 1
            if accum_step % GRAD_ACCUM == 0 or (b_idx + MICRO_BATCH) >= len(epoch_items):
                scaler.unscale_(optimizer)
                torch.nn.utils.clip_grad_norm_(model.parameters(), 1.0)
                scaler.step(optimizer)
                scaler.update()
                scheduler.step()
                optimizer.zero_grad(set_to_none=True)

            epoch_loss += loss.item() * GRAD_ACCUM
            n_batches += 1
            if n_batches % 10 == 0:
                print(
                    f"  epoch {epoch + 1}/{args.epochs} step {n_batches} "
                    f"loss {loss.item() * GRAD_ACCUM:.4f} reward {reward.mean().item():.3f}"
                )

        avg_loss = epoch_loss / max(1, n_batches)
        save_checkpoint(ckpt_dir, model, optimizer, scheduler, epoch, avg_loss, tok, torch)
        print(
            f"train: epoch {epoch + 1}/{args.epochs} done in {time.time() - t0:.0f}s "
            f"(avg loss {avg_loss:.4f}); checkpoint saved"
        )


# ---------------------------------------------------------------------------
# Stage 4: evaluate — base vs fine-tuned on the held-out slice
# ---------------------------------------------------------------------------


def predict_labels(model, items, tok, device, torch):
    model.eval()
    predictions = []
    with torch.no_grad():
        for start in range(0, len(items), 16):
            chunk = items[start : start + 16]
            if not chunk:
                continue
            batch = collate_train_batch(chunk, tok.pad_token_id, torch)
            with amp_context(device):
                logits, _ = model(
                    batch["input_ids"].to(device),
                    batch["attention_mask"].to(device),
                    batch["marker_pos"].to(device),
                    batch["marker_mask"].to(device),
                    batch["qtype"].to(device),
                )
            predictions.extend(logits.float().argmax(-1).cpu().tolist())
    model.train()
    return predictions


def stage_evaluate(args, out_dir, state, device):
    torch = require_heavy()
    from transformers import AutoTokenizer

    eval_path = out_dir / "eval.json"
    if eval_path.exists() and state.get("evaluated_epoch") == args.epochs:
        print("evaluate: already done for this checkpoint")
        return
    cfg = json.loads((out_dir / "base_config.json").read_text())
    model_dir = (out_dir / "base_model_dir").read_text().strip()
    tok = AutoTokenizer.from_pretrained(str(Path(model_dir) / "tokenizer"))
    held_out = torch.load(out_dir / "items.pt", weights_only=False)["held_out"]
    if not held_out:
        print("evaluate: held-out slice is empty, skipping")
        return
    gold = [it["label"] for it in held_out]

    base = load_model(model_dir, cfg, device, torch)
    base_pred = predict_labels(base, held_out, tok, device, torch)
    del base

    tuned = load_model(model_dir, cfg, device, torch, weights_path=str(out_dir / "checkpoint_latest" / "model.safetensors"))
    tuned_pred = predict_labels(tuned, held_out, tok, device, torch)
    del tuned

    labels = ["A", "B", "C", "D"]
    result = {
        "held_out": len(held_out),
        "base_accuracy": sum(p == g for p, g in zip(base_pred, gold)) / len(gold),
        "finetuned_accuracy": sum(p == g for p, g in zip(tuned_pred, gold)) / len(gold),
        "gold_distribution": {labels[g]: gold.count(g) for g in sorted(set(gold))},
    }
    eval_path.write_text(json.dumps(result, indent=2) + "\n")
    print(
        f"evaluate: base {result['base_accuracy']:.3f} -> fine-tuned "
        f"{result['finetuned_accuracy']:.3f} on {len(gold)} held-out items "
        f"(gold: {result['gold_distribution']})"
    )
    state["evaluated_epoch"] = args.epochs


# ---------------------------------------------------------------------------
# Stage 5: finalize — calibration temperatures + final checkpoint directory
# ---------------------------------------------------------------------------

def fit_one_temp(sel, torch):
    """Fit one temperature on held-out (logits, target) pairs; None when too few to fit."""
    from laya.common import clamp_temperature

    if len(sel) < 10:
        return None
    kmax = max(len(z) for z, _ in sel)
    logits = torch.full((len(sel), kmax), -1e4)
    targets = torch.zeros((len(sel), kmax))
    for i, (z, t) in enumerate(sel):
        logits[i, : len(z)] = torch.tensor(z)
        targets[i, : len(t)] = torch.tensor(t, dtype=torch.float32)
    log_t = torch.zeros(1, requires_grad=True)
    opt = torch.optim.LBFGS([log_t], lr=0.1, max_iter=100)

    def closure():
        opt.zero_grad()
        loss = -(targets * torch.log_softmax(logits / log_t.exp(), -1)).sum(-1).mean()
        loss.backward()
        return loss

    opt.step(closure)
    # laya clamps to [TEMP_MIN, TEMP_MAX] at load and warns about anything
    # outside; clamp here so the file holds what laya will apply.
    return clamp_temperature(log_t.exp().item())


def stage_finalize(args, out_dir, state, device):
    torch = require_heavy()
    from transformers import AutoTokenizer
    from safetensors.torch import save_file

    final_dir = out_dir / "model"
    if (final_dir / "rl_agent_config.json").exists() and state.get("finalized_epoch") == args.epochs:
        print("finalize: already done for this checkpoint")
        return
    cfg = json.loads((out_dir / "base_config.json").read_text())
    model_dir = (out_dir / "base_model_dir").read_text().strip()
    tok = AutoTokenizer.from_pretrained(str(Path(model_dir) / "tokenizer"))
    held_out = torch.load(out_dir / "items.pt", weights_only=False)["held_out"]

    model = load_model(model_dir, cfg, device, torch, weights_path=str(out_dir / "checkpoint_latest" / "model.safetensors"))
    model.eval()

    # Fit one temperature per question type on the held-out slice, as the
    # notebook does; temperatures fitted on training items measure the fit,
    # not the calibration.
    calib = []
    with torch.no_grad():
        for start in range(0, len(held_out), 16):
            chunk = held_out[start : start + 16]
            if not chunk:
                continue
            batch = collate_train_batch(chunk, tok.pad_token_id, torch)
            with amp_context(device):
                logits, _ = model(
                    batch["input_ids"].to(device),
                    batch["attention_mask"].to(device),
                    batch["marker_pos"].to(device),
                    batch["marker_mask"].to(device),
                    batch["qtype"].to(device),
                )
            for row, it in zip(logits.float().cpu().tolist(), chunk):
                calib.append((it["qtype"], row[: len(it["markers"])], it["target"]))

    # Types with too few held-out items keep the base checkpoint's fitted value.
    temps = list(cfg.get("temperature", [1.2, 1.2, 1.2]))
    for qtype in range(3):
        sel = [(z, t) for qt, z, t in calib if qt == qtype]
        fitted = fit_one_temp(sel, torch)
        if fitted is not None:
            temps[qtype] = fitted

    final_dir.mkdir(parents=True, exist_ok=True)
    sd = {k: v.half().contiguous().cpu() for k, v in model.state_dict().items()}
    save_file(sd, str(final_dir / "model.safetensors"))
    model.encoder.config.save_pretrained(str(final_dir / "encoder"))
    tok.save_pretrained(str(final_dir / "tokenizer"))
    cfg["fine_tuned"] = True
    cfg["model_name"] = "laya-classifier-smoke"
    cfg["temperature"] = temps
    # This fit is per type; inherited bucket overrides would hide the new values.
    cfg.pop("temperature_by_options", None)
    (final_dir / "rl_agent_config.json").write_text(json.dumps(cfg, indent=2) + "\n")
    print(f"finalize: wrote {final_dir} (temperatures {[round(t, 3) for t in temps]})")
    state["finalized_epoch"] = args.epochs


# ---------------------------------------------------------------------------


def parse_args(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("corpus", nargs="+", type=Path, help="corpus JSONL files")
    parser.add_argument(
        "--out",
        type=Path,
        default=Path("~/.config/antigravity-proxy/finetune").expanduser(),
        help="run directory holding state, checkpoints and the final model",
    )
    parser.add_argument("--epochs", type=int, default=2, help="training epochs (default 2)")
    parser.add_argument("--max-items", type=int, default=0, help="cap exported examples (0 = all)")
    parser.add_argument("--seed", type=int, default=42)
    parser.add_argument("--base-model", default=BASE_MODEL_ID, help="base checkpoint hub id")
    parser.add_argument("--device", choices=("auto", "cuda", "mps", "cpu"), default="auto")
    parser.add_argument("--kind", action="append", choices=KNOWN_KINDS, help="repeatable; default stage1-severity")
    parser.add_argument("--source", action="append", choices=KNOWN_SOURCES, help="repeatable; default upstream")
    parser.add_argument("--fresh", action="store_true", help="wipe the run directory's state first")
    parser.add_argument("--dry-run", action="store_true", help="export only; print the plan, train nothing")
    return parser.parse_args(argv)


def main(argv=None):
    args = parse_args(argv)
    args.out.mkdir(parents=True, exist_ok=True)
    if args.fresh:
        for name in ("state.json", "dataset.jsonl", "base_config.json", "base_model_dir"):
            (args.out / name).unlink(missing_ok=True)
        discard_progress(args.out)

    state = load_state(args.out)
    signature = config_signature(args)
    digest = corpus_sha256(args.corpus)
    changed = [
        name
        for name, same in (
            ("configuration", state.get("config") == signature),
            ("corpus", state.get("corpus_sha256") == digest),
        )
        if not same
    ]
    # A dry run over stale state only previews: it neither discards progress
    # nor records the new corpus/config, which would let the next real run
    # resume the old checkpoint against them.
    preview = bool(state and changed and args.dry_run)
    if state and changed:
        what = " and ".join(changed)
        if preview:
            print(f"dry-run: {what} changed; a real run discards the saved progress and trains from the base checkpoint")
        else:
            print(f"{what} changed; training restarts from the base checkpoint")
            discard_progress(args.out)
        state = {}

    examples = stage_export(args, args.out, state, digest, write=not preview)
    if not preview:
        state["corpus_sha256"] = digest
        state["config"] = signature
        save_state(args.out, state)

    if args.dry_run:
        device_note = "auto (cuda -> mps -> cpu)" if args.device == "auto" else args.device
        print(f"dry-run: would preprocess, train {args.epochs} epochs, evaluate and finalize")
        print(f"dry-run: out={args.out} device={device_note}")
        return 0

    torch = require_heavy()
    device = resolve_device(args.device, torch)
    print(f"device: {device}")

    stage_preprocess(args, args.out, state, examples, digest)
    state["items_sha256"] = digest
    save_state(args.out, state)

    stage_train(args, args.out, state, device)
    stage_evaluate(args, args.out, state, device)
    stage_finalize(args, args.out, state, device)
    save_state(args.out, state)
    print("done.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
