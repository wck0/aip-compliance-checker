#!/usr/bin/env python3
"""
Claude PR Reviewer - Automatically reviews pull requests using the Claude API
and posts feedback as GitHub comments.
"""

import os
import json
import time
from collections import namedtuple
import anthropic
from anthropic import Anthropic
import openai
from openai import OpenAI
from github import Github

MAX_RETRIES = 5
INITIAL_BACKOFF_SECONDS = 5
MAX_BACKOFF_SECONDS = 60
MAX_TOKENS = 2000

OPENROUTER_BASE_URL = "https://openrouter.ai/api/v1"
OPENROUTER_MODEL = "anthropic/claude-opus-4-6"
ANTHROPIC_MODEL = "claude-opus-4-6"

Backend = namedtuple("Backend", ["kind", "client", "model"])


class ClaudeAPIUnavailableError(Exception):
    """Raised when the Claude API is unreachable after retries (rate limit or transient outage)."""

    def __init__(self, reason, original):
        self.reason = reason
        self.original = original
        super().__init__(f"{reason}: {original}")


def _response_headers(error):
    response = getattr(error, "response", None)
    return getattr(response, "headers", None) if response is not None else None


def _retry_after_seconds(error):
    """Honor the Anthropic `retry-after` header when present on a rate-limit response."""
    headers = _response_headers(error)
    value = headers.get("retry-after") if headers else None
    if not value:
        return None
    try:
        return max(0, int(float(value)))
    except (TypeError, ValueError):
        return None


def _format_rate_limit_diagnostics(error):
    """Pull the error body and any ratelimit-related headers off a 429 for logging."""
    parts = [f"error={error}"]
    headers = _response_headers(error) or {}
    interesting = sorted(
        (k, v) for k, v in headers.items()
        if "ratelimit" in k.lower() or k.lower() == "retry-after"
    )
    if interesting:
        parts.append("headers={" + ", ".join(f"{k}={v}" for k, v in interesting) + "}")
    return " | ".join(parts)


def _call_with_retries(call_fn):
    """Invoke call_fn, retrying on rate limits and transient server errors from either backend."""
    for attempt in range(MAX_RETRIES + 1):
        try:
            return call_fn()
        except (anthropic.RateLimitError, openai.RateLimitError) as e:
            diagnostics = _format_rate_limit_diagnostics(e)
            if attempt == MAX_RETRIES:
                print(f"❌ Rate-limited after {MAX_RETRIES + 1} attempts | {diagnostics}")
                raise ClaudeAPIUnavailableError("rate limited", e) from e
            wait = _retry_after_seconds(e) or min(
                INITIAL_BACKOFF_SECONDS * (2 ** attempt), MAX_BACKOFF_SECONDS
            )
            print(
                f"⏳ Rate-limited (attempt {attempt + 1}/{MAX_RETRIES + 1}); "
                f"sleeping {wait}s | {diagnostics}"
            )
        except (anthropic.APIStatusError, openai.APIStatusError) as e:
            status = getattr(e, "status_code", None)
            if status is None or status < 500:
                raise
            if attempt == MAX_RETRIES:
                raise ClaudeAPIUnavailableError(f"API status {status}", e) from e
            wait = min(INITIAL_BACKOFF_SECONDS * (2 ** attempt), MAX_BACKOFF_SECONDS)
            print(f"⏳ API error {status} (attempt {attempt + 1}/{MAX_RETRIES + 1}); sleeping {wait}s")
        except (anthropic.APIConnectionError, openai.APIConnectionError) as e:
            if attempt == MAX_RETRIES:
                raise ClaudeAPIUnavailableError("connection error", e) from e
            wait = min(INITIAL_BACKOFF_SECONDS * (2 ** attempt), MAX_BACKOFF_SECONDS)
            print(f"⏳ Connection error (attempt {attempt + 1}/{MAX_RETRIES + 1}); sleeping {wait}s")
        time.sleep(wait)
    raise RuntimeError("Retry loop exited without returning")


def _select_backend():
    """Pick a chat backend; OpenRouter wins when its key is set, else fall back to the Anthropic OAuth path."""
    openrouter_key = os.getenv("OPENROUTER_API_KEY")
    if openrouter_key:
        print(f"🔌 Using OpenRouter backend (model={OPENROUTER_MODEL})")
        client = OpenAI(api_key=openrouter_key, base_url=OPENROUTER_BASE_URL, max_retries=0)
        return Backend(kind="openrouter", client=client, model=OPENROUTER_MODEL)
    oauth_token = os.getenv("CLAUDE_CODE_OAUTH_TOKEN")
    if oauth_token:
        print(f"🔌 Using Anthropic OAuth backend (model={ANTHROPIC_MODEL})")
        client = Anthropic(auth_token=oauth_token, max_retries=0)
        return Backend(kind="anthropic", client=client, model=ANTHROPIC_MODEL)
    raise RuntimeError("Neither OPENROUTER_API_KEY nor CLAUDE_CODE_OAUTH_TOKEN is set")


def _invoke_chat(backend, system_prompt, user_message):
    """Call the selected backend with retries and return the assistant's text."""
    if backend.kind == "openrouter":
        def call():
            return backend.client.chat.completions.create(
                model=backend.model,
                max_tokens=MAX_TOKENS,
                messages=[
                    {"role": "system", "content": system_prompt},
                    {"role": "user", "content": user_message},
                ],
            )
        resp = _call_with_retries(call)
        return resp.choices[0].message.content

    def call():
        return backend.client.messages.create(
            model=backend.model,
            max_tokens=MAX_TOKENS,
            system=system_prompt,
            messages=[{"role": "user", "content": user_message}],
        )
    resp = _call_with_retries(call)
    return resp.content[0].text

def get_pr_diff(gh_client, repo_owner, repo_name, pr_number):
    """Fetch the diff for a pull request."""
    repo = gh_client.get_repo(f"{repo_owner}/{repo_name}")
    pr = repo.get_pull(pr_number)
    
    # Get all commits in the PR
    commits = pr.get_commits()
    
    # Build diff content
    diff_content = f"# PR #{pr_number}: {pr.title}\n\n"
    diff_content += f"**Author:** {pr.user.login}\n"
    diff_content += f"**Description:** {pr.body or '(No description provided)'}\n\n"
    diff_content += "## Changed Files\n\n"
    
    files = pr.get_files()
    total_changes = 0
    
    for file in files:
        if file.changes > 0:  # Only include changed files
            diff_content += f"### {file.filename} ({file.changes} changes)\n"
            diff_content += f"- **Additions:** {file.additions}\n"
            diff_content += f"- **Deletions:** {file.deletions}\n\n"
            
            if file.patch:
                diff_content += f"```diff\n{file.patch}\n```\n\n"
            
            total_changes += file.changes
    
    return diff_content, pr, total_changes


def load_review_skill(repo_path="."):
    """Load the AI review skill from the repository."""
    skill_path = os.path.join(repo_path, ".claude", "skills", "aip-review", "SKILL.md")

    if os.path.exists(skill_path):
        print(f"📚 Loading review skill from {skill_path}")
        with open(skill_path, "r") as f:
            return f.read()
    else:
        print(f"⚠️  Skill file not found at {skill_path}, using default prompt")
        return None


def load_proto_context(proto_context_path, proto_repo=None, proto_ref=None):
    """Load all .proto files from the checked-out proto repo as review context."""
    if not proto_context_path or not os.path.isdir(proto_context_path):
        print(f"⚠️  Proto context path not found: {proto_context_path}")
        return None

    skip_dirs = {"vendor", "node_modules", ".git", "third_party"}
    proto_files = []
    for root, dirs, files in os.walk(proto_context_path):
        dirs[:] = [d for d in dirs if d not in skip_dirs]
        for name in files:
            if not name.endswith(".proto"):
                continue
            full = os.path.join(root, name)
            rel = os.path.relpath(full, proto_context_path)
            with open(full, "r", encoding="utf-8", errors="replace") as fh:
                proto_files.append((rel, fh.read()))

    if not proto_files:
        print(f"⚠️  No .proto files found under {proto_context_path}")
        return None

    proto_files.sort(key=lambda x: x[0])
    source = f"{proto_repo}@{proto_ref}" if proto_repo and proto_ref else proto_context_path
    print(f"📜 Loaded {len(proto_files)} .proto files from {source}")

    parts = [
        "## Proto Definitions Context",
        "",
        f"The following `.proto` files are provided from `{source}` as the authoritative "
        "API surface definitions for this review. Treat them as the source of truth when "
        "evaluating whether the Go code in the PR diff is AIP-compliant — cross-reference "
        "Go types, methods, and field names against these protos.",
        "",
    ]
    for rel, body in proto_files:
        parts.append(f"### `{rel}`")
        parts.append("")
        parts.append("```proto")
        parts.append(body.rstrip())
        parts.append("```")
        parts.append("")
    return "\n".join(parts)


def review_code_with_claude(diff_content, total_changes, repo_path=".", proto_context=None):
    """Use Claude (via OpenRouter or Anthropic OAuth) to review the PR diff."""
    backend = _select_backend()

    # Try to load custom skill from repository
    custom_skill = load_review_skill(repo_path)
    
    if custom_skill:
        system_prompt = custom_skill
    else:
        # Fallback to default prompt if skill not found
        system_prompt = """You are an expert code reviewer. Analyze the provided pull request diff and provide:

1. **Summary**: A brief overview of what changes were made
2. **Strengths**: What's good about this code
3. **Issues Found**: Any bugs, security concerns, performance issues, or code quality problems (if none, say so)
4. **Suggestions**: Specific recommendations for improvement
5. **Rating**: A brief assessment (e.g., "Looks good to merge", "Needs minor fixes", "Needs significant changes")

Be constructive, specific, and actionable. Focus on:
- Code correctness and potential bugs
- Security vulnerabilities
- Performance concerns
- Code style and maintainability
- Testing adequacy (if visible)

Format your response as clear sections with markdown. Be concise but thorough."""

    pr_section = f"""Please review this pull request:

{diff_content}

Total lines changed: {total_changes}"""

    if proto_context:
        user_message = f"{proto_context}\n\n---\n\n{pr_section}"
    else:
        user_message = pr_section

    return _invoke_chat(backend, system_prompt, user_message)


def post_review_comment(gh_client, repo_owner, repo_name, pr_number, review_text):
    """Post the Claude review as a GitHub comment."""
    repo = gh_client.get_repo(f"{repo_owner}/{repo_name}")
    pr = repo.get_pull(pr_number)

    # Format the comment with a bot header
    comment_body = f"""## 🤖 Claude Code Review

{review_text}

---
*This review was automatically generated by Claude. Please review and use your judgment when applying suggestions.*"""

    pr.create_issue_comment(comment_body)
    print(f"✅ Posted review comment to PR #{pr_number}")


def post_unavailable_comment(gh_client, repo_owner, repo_name, pr_number, reason):
    """Post a soft-fail comment when Claude is unreachable after retries."""
    repo = gh_client.get_repo(f"{repo_owner}/{repo_name}")
    pr = repo.get_pull(pr_number)
    comment_body = f"""## 🤖 Claude Code Review — Skipped

The Claude API was unavailable after {MAX_RETRIES + 1} attempts ({reason}).

Re-run the **Claude Code Review** workflow on this PR to try again.

---
*This is an automated notice. No review was generated.*"""
    pr.create_issue_comment(comment_body)
    print(f"ℹ️  Posted unavailable notice to PR #{pr_number}")


def main():
    # Get environment variables
    openrouter_key = os.getenv("OPENROUTER_API_KEY")
    oauth_token = os.getenv("CLAUDE_CODE_OAUTH_TOKEN")
    github_token = os.getenv("GITHUB_TOKEN")
    pr_number = int(os.getenv("PR_NUMBER"))
    repo_owner = os.getenv("REPO_OWNER")
    repo_name = os.getenv("REPO_NAME")

    if not (openrouter_key or oauth_token):
        print("❌ Need either OPENROUTER_API_KEY or CLAUDE_CODE_OAUTH_TOKEN")
        return
    if not all([github_token, pr_number, repo_owner, repo_name]):
        print("❌ Missing required environment variables")
        return
    
    print(f"🔍 Reviewing PR #{pr_number} in {repo_owner}/{repo_name}...")
    
    # Initialize GitHub client
    gh_client = Github(github_token)
    
    try:
        # Fetch PR diff
        print("📥 Fetching PR diff...")
        diff_content, pr, total_changes = get_pr_diff(gh_client, repo_owner, repo_name, pr_number)
        
        if total_changes == 0:
            print("⏭️  No changes found in PR, skipping review")
            return
        
        print(f"📊 Found {total_changes} lines changed across {len(list(pr.get_files()))} files")

        # Load proto context if the workflow checked out a proto repo for us
        proto_context_path = os.getenv("PROTO_CONTEXT_PATH")
        proto_repo = os.getenv("PROTO_REPO")
        proto_ref = os.getenv("PROTO_REF")
        proto_context = None
        if proto_context_path:
            print(f"📂 Loading proto context from {proto_repo}@{proto_ref} ({proto_context_path})")
            proto_context = load_proto_context(proto_context_path, proto_repo, proto_ref)

        # Get Claude's review
        print("🤔 Getting Claude's review...")
        review_text = review_code_with_claude(
            diff_content, total_changes, repo_path=".", proto_context=proto_context
        )
        
        # Post the review
        print("📤 Posting review to GitHub...")
        post_review_comment(gh_client, repo_owner, repo_name, pr_number, review_text)
        
        print("✨ Review complete!")

    except ClaudeAPIUnavailableError as e:
        print(f"⚠️  Claude API unavailable after retries ({e.reason}); posting soft-fail notice")
        try:
            post_unavailable_comment(gh_client, repo_owner, repo_name, pr_number, e.reason)
        except Exception as comment_err:
            print(f"❌ Failed to post unavailable notice: {comment_err}")
            raise
    except Exception as e:
        print(f"❌ Error during review: {str(e)}")
        raise


if __name__ == "__main__":
    main()