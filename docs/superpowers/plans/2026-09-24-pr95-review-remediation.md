# PR #95 review remediation: laya smoke fine-tune

**Branch:** `feat/laya-finetune-smoke` (head `d1a8886`)
**Base:** `main`
**PR:** https://github.com/gustavokch/antigravity-claude-proxy-go/pull/95
**Review comment:** https://github.com/gustavokch/antigravity-claude-proxy-go/pull/95#issuecomment-5821943888

## Goal

Close all ten review findings. Before the review was posted, both 🔴 findings and the unclosed-thinking 🟡 were reproduced on `d1a8886` using a CPU smoke run of the full pipeline. The run used a tiny laya-shaped base model with 3.4M params, and `snapshot_download` was redirected to it.

| # | Sev | Finding | Repro observed |
|---|-----|---------|----------------|
| 1 | 🔴 | Corpus change keeps stale checkpoint/eval | `preprocess: 198 train + 21 held-out` then `train: checkpoint already at epoch 2/2, nothing to do` |
| 2 | 🔴 | `--dry-run` discards progress on config change | `--dry-run --epochs 3` removed `checkpoint_latest/`, `items.pt`, `eval.json` |
| 3 | 🟡 | Tag quoted in truncated `<thinking>` recovered | `recover_severity({"verdict_raw": "<thinking>… <severity>90"})` → `90` |
| 4 | 🟡 | Rolling checkpoint overwritten in place | n/a (crash window) |
| 5 | 🟡 | Rolling checkpoint stored fp16 | n/a (precision) |
| 6 | 🟡 | Unfiltered `snapshot_download` (~2.3 GB) | smoke log: `snapshot calls: [('convaiinnovations/laya', {})]` |
| 7 | 🔵 | Cosine `T_max` smaller than the number of optimizer steps | LR 1.00e-6 → 1.10e-6 after `T_max` 48 of 50 steps |
| 8 | 🔵 | Temperatures reset to 1.2 / clamped outside laya's range | n/a |
| 9 | 🔵 | Exporter docstring still says upstream-only | n/a |
| 10 | 🔵 | Test named "invalidates progress" compares signatures only | n/a |

## Architecture

- `internal/classifier/corpus/verdict.go` `ParseVerdict`: reads severity and category from `answer`, which is the text with `<thinking>` spans removed. The capture recorder (`BuildEntry`) is the only production caller, so the change affects captured labels and never a live verdict.
- `scripts/corpus_to_laya.py` `recover_severity`: the export-time Python mirror of the unclosed fallback, used for rows captured before the Go fix.
- `scripts/finetune_laya.py` `main`: loads the state, resets it if stale, runs export, saves the state, then runs preprocess → train → evaluate → finalize. Each stage skips itself by checking `state.json` and the files it produced.

## Tech stack

Go 1.27rc2 stdlib (`go test ./internal/classifier/corpus/`). Python 3 with pytest (`python3 -m pytest scripts/`). The pure-python tests import no heavy dependencies. The heavy stages are smoke-verified in a throwaway venv (`/tmp/laya-venv`: laya 0.3.20, torch 2.14, transformers 5.17) against the tiny base model.

## Spec reference

- PR #95 body: "thinking spans stay stripped, so a quoted tag never becomes a verdict"; "a re-run resumes at the next epoch; config changes restart from base".
- `docs/classifier-rules.md` § Smoke-test fine-tune: `--dry-run` "runs only the export stage and prints the plan"; "a crash mid-training loses at most the current epoch".
- laya 0.3.20 `agent.py:261-268` (root-checkpoint `allow_patterns`) and `common.py:311-323` (`clamp_temperature`, [0.5, 5.0]).

---

## Task 1: ignore a truncated `<thinking>` when reading the verdict (finding 3, 9)

**Files:** Modify `internal/classifier/corpus/verdict.go`, `scripts/corpus_to_laya.py`. Test `internal/classifier/corpus/verdict_test.go`, `scripts/test_corpus_to_laya.py`.
**Consumes:** nothing new. **Produces:** `ParseVerdict` and `recover_severity` also ignore everything from an unclosed `<thinking>` to the end of the text.

Step 1: add failing tests.

```go
{
	// A teacher cut off inside its rationale left no verdict at all.
	name:     "unclosed tag inside truncated thinking is not recovered",
	text:     "<thinking>A force push would be <severity>90",
	severity: -1,
},
{
	name:     "closed tag inside truncated thinking is not a verdict",
	text:     "<thinking>A force push would be <severity>90</severity>, but",
	severity: -1,
},
```

```python
def test_convert_does_not_recover_a_tag_in_truncated_thinking():
    rows = [
        {"action": "a", "severity": -1, "verdict_raw": "<thinking>A force push would be <severity>90",
         "source": "upstream", "kind": STAGE1},
    ]
    examples, stats = convert(rows)
    assert examples == []
    assert stats["recovered"] == 0
```

Step 2: `go test ./internal/classifier/corpus/ -run TestParseVerdict` → FAIL (`Severity = 90, want -1`). `python3 -m pytest scripts/test_corpus_to_laya.py -q` → FAIL.

Step 3: in Go, add `unclosedThinkingPattern = regexp.MustCompile(`(?s)<thinking>.*$`)` and compute `answer` by removing the closed spans first and then the unclosed tail. In Python, add `UNCLOSED_THINKING_PATTERN = re.compile(r"<thinking>.*\Z", re.DOTALL)` and apply it after `THINKING_PATTERN`. Fix the docstring (finding 9).

Step 4: re-run both commands → PASS.

Step 5: `git commit -m "fix(classifier): ignore tags inside truncated thinking"`.

## Task 2: a changed corpus discards progress; dry-run never does (findings 1, 2, 10)

**Files:** Modify `scripts/finetune_laya.py` (`corpus_sha256`, `stage_export`, `main`, new `discard_progress`), `docs/classifier-rules.md`. Test `scripts/test_finetune_laya.py`.
**Produces:** `discard_progress(out_dir)`. `stage_export(args, out_dir, state, digest, write=True) -> examples`.

Step 1: add failing tests. A monkeypatched `require_heavy` stops a real run at the torch boundary, after the reset and export have run.

```python
class _HeavyBoundary(Exception):
    pass


def _stop_at_heavy(monkeypatch):
    def boundary():
        raise _HeavyBoundary
    monkeypatch.setattr(finetune_laya, "require_heavy", boundary)


def _seed_progress(out):
    (out / "checkpoint_latest").mkdir()
    (out / "checkpoint_latest" / "checkpoint_meta.json").write_text('{"epoch": 2}')
    (out / "items.pt").write_bytes(b"items")
    (out / "eval.json").write_text("{}")
    (out / "model").mkdir()


def _progress(out):
    return sorted(name for name in ("checkpoint_latest", "items.pt", "eval.json", "model") if (out / name).exists())


def test_corpus_change_discards_progress(tmp_path, monkeypatch):
    # corpus with one upstream row; run once, seed progress, append a row, run again
    ...
    assert _progress(out) == []


def test_config_change_discards_progress(tmp_path, monkeypatch):  # replaces the signature-only test
    ...
    assert _progress(out) == []


def test_dry_run_keeps_progress_when_the_config_changed(tmp_path, monkeypatch):
    ...
    assert _progress(out) == ["checkpoint_latest", "eval.json", "items.pt", "model"]
    assert load_state(out)["config"]["epochs"] == 2


def test_corpus_sha256_ignores_how_a_path_is_spelled(tmp_path, monkeypatch):
    monkeypatch.chdir(tmp_path)
    ...
    assert corpus_sha256([Path("c.jsonl")]) == corpus_sha256([tmp_path / "c.jsonl"])
```

Step 2: `python3 -m pytest scripts/test_finetune_laya.py -q`. The corpus-change, dry-run and path tests fail, and so does the config-change test, because `model/` survives.

Step 3: `main` computes the digest up front. When the state is non-empty and either the config or the corpus digest changed, a real run prints the notice and calls `discard_progress`, and a dry run prints what a real run would do. In both cases the export then runs from an empty state, and a dry run writes nothing (`write=False`) and does not save the state. `corpus_sha256` hashes `Path(p).resolve()`. `--fresh` reuses `discard_progress`. Update the docs paragraph: a changed corpus restarts training, the live day file grows, so copy it to resume across runs, and `--dry-run` deletes nothing.

Step 4: re-run → PASS. Smoke: append a row to the pinned corpus copy and expect `corpus changed; training restarts from the base checkpoint` followed by a full two-epoch train. `--dry-run --epochs 3` must leave `checkpoint_latest/` in place.

Step 5: `git commit -m "fix(scripts): restart fine-tune on corpus change"`.

## Task 3: commit the rolling checkpoint atomically, at full precision (findings 4, 5)

**Files:** Modify `scripts/finetune_laya.py` (`save_checkpoint`, `stage_train`, `discard_progress`, new `commit_checkpoint`/`recover_checkpoint`). Test `scripts/test_finetune_laya.py`.
**Produces:** `commit_checkpoint(ckpt_dir)`, which swaps in `<ckpt>.staging` and uses `<ckpt>.old` for the outgoing copy, and `recover_checkpoint(ckpt_dir)`, which finishes or rolls back an interrupted swap. The staging dir's meta file is written last and acts as its commit marker.

Step 1: failing tests (pure file operations).

```python
def test_recover_checkpoint_discards_an_incomplete_staging_dir(tmp_path):
    # latest at epoch 1; staging holds weights but no meta (crash mid-write)
    ...
    assert _epoch(latest) == 1 and not staging.exists()


def test_recover_checkpoint_promotes_a_complete_staging_dir(tmp_path):
    # crash between the renames: latest missing, .old at epoch 1, staging complete at epoch 2
    ...
    assert _epoch(latest) == 2 and not staging.exists() and not retired.exists()


def test_commit_checkpoint_replaces_the_previous_one(tmp_path):
    ...
    assert _epoch(latest) == 2 and not staging.exists() and not retired.exists()
```

Step 2: pytest → FAIL (ImportError).

Step 3: implement both helpers. `save_checkpoint` writes into staging, meta last, then commits, and keeps weights at the model's own dtype (fp32). `stage_train` calls `recover_checkpoint` before reading the meta. `discard_progress` also removes the staging and `.old` dirs. `finalize` keeps writing fp16.

Step 4: pytest → PASS. Smoke: `checkpoint_latest/model.safetensors` tensors are `float32`, `model/model.safetensors` tensors are `float16`, and a resume from a hand-set epoch 1 still completes.

Step 5: `git commit -m "fix(scripts): atomic fp32 rolling checkpoint"`.

## Task 4: download only the root checkpoint (finding 6)

**Files:** Modify `scripts/finetune_laya.py` (`BASE_MODEL_FILES`, `stage_preprocess`).

No unit test: it could only assert that a keyword gets forwarded. Throwaway verification instead: run `HfApi().list_repo_files` + `fnmatch` over `BASE_MODEL_FILES` and confirm it keeps only the root checkpoint files (≈0.85 GB of the ≈2.3 GB). Also confirm that the smoke log shows `allow_patterns` in the snapshot call.

Commit: `git commit -m "fix(scripts): fetch only the root laya checkpoint"`.

## Task 5: scheduler horizon and base temperatures (findings 7, 8)

**Files:** Modify `scripts/finetune_laya.py` (new `optimizer_steps`, `stage_train`, `fit_one_temp`, `stage_finalize`). Test `scripts/test_finetune_laya.py`.

Step 1: failing test. The partial batch at the end of each epoch also steps the scheduler.

```python
def test_optimizer_steps_counts_the_partial_last_batch():
    assert optimizer_steps(198, 2) == 50
    assert optimizer_steps(16, 1) == 2
```

Step 2: pytest → FAIL (ImportError).

Step 3: `optimizer_steps(n_items, epochs) = ceil(n_items / (MICRO_BATCH * GRAD_ACCUM)) * epochs`, used as `T_max`. `fit_one_temp` returns `None` below 10 items and otherwise clamps with `laya.common.clamp_temperature`. `stage_finalize` starts from `cfg["temperature"]` and overwrites only the qtypes it fitted.

Step 4: pytest → PASS. The smoke finalize line shows base temperatures for the qtypes that were not fitted (`1.251`, `1.983`).

Step 5: `git commit -m "fix(scripts): cosine horizon and base temperatures"`.

## Verification

1. `go test ./internal/classifier/... ./internal/api/` and `python3 -m pytest scripts/` must be green.
2. Full CPU smoke on the tiny base: fresh run, corpus-change restart, dry-run with a config change, and resume from epoch 1.
3. `git push fork feat/laya-finetune-smoke`, then reply on the review comment mapping each finding to its commit.
