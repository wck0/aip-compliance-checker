# AIP Compliance Checker

Automated AIP compliance reviews for Go API pull requests, cross-referenced against your protobuf source of truth.

## Demo

[Watch the demo on asciinema](https://asciinema.org/a/TsVfvHEYpUnDFFRw) — a non-compliant Go file opens a PR, the workflow runs end-to-end, and the AI posts an inline review naming the model that produced it.

## What it does

A reusable GitHub Actions workflow that reviews pull requests for compliance with Google's [API Improvement Proposals (AIPs)](https://aip.dev/).

When a PR opens (or is updated), the action:

1. Checks out the corresponding branch from your protobuf schema repo, matched by ticket prefix (e.g., a PR branch `lndeng-1234-add-billing` resolves to a `lndeng-1234-*` branch in the proto repo, falling back to `main`).
2. Bundles the PR diff together with the full proto context into a single prompt.
3. Sends it to a chat LLM via [OpenRouter](https://openrouter.ai/) — default `google/gemma-4-31b-it`, configurable per repo.
4. Posts the structured AIP review back to the PR as an inline comment, with the model name in the header so reviewers can calibrate confidence.

Built for resilience: exponential-backoff retries on rate limits, vendor-agnostic backend with OpenRouter as primary and Anthropic Claude as opt-in fallback, and a soft-fail path that posts a "re-run me" notice instead of breaking PRs when every backend is unavailable.

## Quick start

Add this to `.github/workflows/aip-review.yml` in your consumer repo:

```yaml
name: AIP Review

on:
  pull_request:
    types: [opened, synchronize, reopened]

jobs:
  review:
    uses: wck0/aip-compliance-checker/.github/workflows/claude-review.yml@main
    secrets:
      OPENROUTER_API_KEY: ${{ secrets.OPENROUTER_API_KEY }}
      PROTO_REPO_TOKEN: ${{ secrets.PROTO_REPO_TOKEN }}   # if your proto repo is private
    with:
      proto_repo: my-org/my-protos                        # optional, defaults to canonical/landscape-proto
      openrouter_model: anthropic/claude-sonnet-4.6       # optional, defaults to google/gemma-4-31b-it
```

## Secrets

| Secret | Required | Purpose |
|---|---|---|
| `OPENROUTER_API_KEY` | yes (default backend) | Auth for the OpenRouter chat-completions endpoint |
| `CLAUDE_CODE_OAUTH_TOKEN` | only with `use_anthropic: true` | Auth for the Anthropic API fallback |
| `PROTO_REPO_TOKEN` | only if the proto repo is private | GitHub token with `repo:read` on the proto repo |

At least one of `OPENROUTER_API_KEY` or `CLAUDE_CODE_OAUTH_TOKEN` (with `use_anthropic: true`) must be set, otherwise the workflow fails fast.

## Inputs

| Input | Default | Description |
|---|---|---|
| `proto_repo` | `canonical/landscape-proto` | Repo containing the `.proto` definitions used as review context (`owner/repo`) |
| `openrouter_model` | `google/gemma-4-31b-it` | OpenRouter model slug. Examples: `google/gemma-4-31b-it`, `openai/gpt-5.1-codex-max`, `google/gemini-2.5-pro` |
| `use_anthropic` | `false` | Enable Anthropic as a fallback when OpenRouter is unavailable |
| `script_repo` | `wck0/aip-compliance-checker` | Repo from which to download the review script and skill |
| `script_ref` | `main` | Branch/tag/ref to download the script from |

## Model selection

The reviewer is vendor-agnostic — any model on OpenRouter that supports chat completions works. Some practical picks:

| Slug | Context | Use case |
|---|---|---|
| `google/gemma-4-31b-it` | 128K | Cheap default, open-weight model |
| `anthropic/claude-sonnet-4.6` | 1M | Strong code review, mid-tier pricing |
| `anthropic/claude-opus-4.7` | 1M | Highest-quality reviews, premium pricing |
| `openai/gpt-5.1-codex-max` | 400K | OpenAI's strongest coding model |
| `google/gemini-2.5-pro` | 1M | Closed Google frontier model |

To discover what your key has access to, query the OpenRouter catalog:

```bash
curl -s -H "Authorization: Bearer $OPENROUTER_API_KEY" https://openrouter.ai/api/v1/models \
  | jq -r '.data[] | "\(.id)\t\(.context_length)"'
```

## Local development

Clone the repo and run the demo script for an end-to-end test (opens a PR with a deliberately AIP-noncompliant Go file, watches the workflow, prints the posted review):

```bash
./scripts/demo.sh
```

The script pre-flights `gh` auth and required secrets, creates a `demo/aip-violations-<timestamp>` branch, and prints a cleanup command at the end.

Recording the demo for a screen capture? See the [asciinema documentation](https://docs.asciinema.org/) — pair `asciinema rec` with [`agg`](https://github.com/asciinema/agg) to produce a shareable GIF or MP4.

## How the review is generated

The script in `.github/scripts/claude_review.py` builds the prompt from two sources:

- The PR diff, including title, description, and all changed files with their patches.
- The proto context — every `.proto` file in the matched proto-repo branch, concatenated as the source of truth for API surface.

The system prompt is loaded from `.claude/skills/aip-review/SKILL.md`, which encodes the specific AIPs the reviewer should check (AIP-127 naming, AIP-131 standard methods, AIP-132 list, AIP-135 delete, etc.).

The output is a Markdown comment with sections for summary, issues found, suggestions, and an overall rating. The comment header identifies the exact model that produced the review.

## License

See [LICENSE](LICENSE).
