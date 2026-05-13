#!/usr/bin/env bash
# Drives an end-to-end demo of the AIP compliance checker workflow:
#   1. Makes a branch with a deliberately AIP-noncompliant Go file
#   2. Opens a PR
#   3. Watches the workflow run
#   4. Prints the review comment the bot posted
#
# Intended to be screen-recorded in one take. Pacing has small sleeps so a
# viewer can follow along.
#
# Preconditions:
#   - gh CLI installed and authenticated against this repo
#   - Working tree clean
#   - OPENROUTER_API_KEY (or CLAUDE_CODE_OAUTH_TOKEN) is set as a repo secret
#   - PROTO_REPO_TOKEN is set if the configured proto repo is private

set -euo pipefail

DEMO_BRANCH="${DEMO_BRANCH:-demo/aip-violations-$(date +%s)}"
BASE_BRANCH="${BASE_BRANCH:-main}"
DEMO_FILE="${DEMO_FILE:-demo/user_service.go}"
WORKFLOW_FILE="${WORKFLOW_FILE:-claude-review.yml}"
PAUSE="${PAUSE:-2}"

step() { printf "\n\033[1;36m▶ %s\033[0m\n" "$*"; }
info() { printf "  %s\n" "$*"; }
fail() { printf "\n\033[1;31m✗ %s\033[0m\n" "$*" >&2; exit 1; }

preflight() {
    command -v gh >/dev/null || fail "gh CLI not installed (https://cli.github.com)"
    gh auth status >/dev/null 2>&1 || fail "gh not authenticated; run 'gh auth login'"
    git rev-parse --is-inside-work-tree >/dev/null 2>&1 || fail "not in a git repo"
    [[ -z "$(git status --porcelain)" ]] || fail "working tree not clean; commit/stash first"
    git fetch origin "$BASE_BRANCH" >/dev/null 2>&1 || fail "could not fetch origin/$BASE_BRANCH"

    if ! gh secret list 2>/dev/null | grep -qE '^(OPENROUTER_API_KEY|CLAUDE_CODE_OAUTH_TOKEN)\b'; then
        info "⚠️  Neither OPENROUTER_API_KEY nor CLAUDE_CODE_OAUTH_TOKEN appears in 'gh secret list'."
        info "    The workflow run will fail unless one of them is configured."
        read -r -p "    Continue anyway? [y/N] " ans
        [[ "$ans" =~ ^[yY] ]] || exit 1
    fi
}

write_demo_file() {
    mkdir -p "$(dirname "$DEMO_FILE")"
    cat > "$DEMO_FILE" <<'EOF'
// Demo file: intentional AIP violations for the review workflow to flag.
// DO NOT use this as a reference — it is non-compliant on purpose.
package users

import "context"

type User struct {
    ID   string
    Name string
}

type UserService struct{}

// AIP-127 violation: method name is snake_case and non-standard.
// AIP-131 violation: Get methods must return the singular resource.
func (s *UserService) fetch_user(ctx context.Context, id string) ([]*User, error) {
    return nil, nil
}

// AIP-132 violation: list methods must be named ListUsers and accept pagination.
func (s *UserService) RetrieveAllUsers(ctx context.Context) ([]*User, error) {
    return nil, nil
}

// AIP-135 violation: delete should be named DeleteUser and return Empty (or the soft-deleted resource only for soft-delete).
func (s *UserService) RemoveUser(ctx context.Context, id string) (*User, error) {
    return nil, nil
}
EOF
}

main() {
    step "AIP Compliance Checker — Demo"
    info "branch: $DEMO_BRANCH"
    info "base:   $BASE_BRANCH"
    info "file:   $DEMO_FILE"
    sleep "$PAUSE"

    step "Pre-flight checks"
    preflight
    info "✓ gh authenticated, working tree clean, secrets look OK"
    sleep "$PAUSE"

    step "Creating branch off origin/$BASE_BRANCH"
    git checkout -b "$DEMO_BRANCH" "origin/$BASE_BRANCH"
    sleep 1

    step "Dropping in an intentionally non-compliant Go file"
    write_demo_file
    git --no-pager diff --no-color -- "$DEMO_FILE" | head -60
    sleep "$PAUSE"

    step "Committing and pushing"
    git add "$DEMO_FILE"
    git commit -m "demo: add user service with deliberate AIP violations"
    git push -u origin "$DEMO_BRANCH"
    sleep 1

    step "Opening PR"
    PR_URL=$(gh pr create \
        --base "$BASE_BRANCH" \
        --head "$DEMO_BRANCH" \
        --title "demo: user service (AIP violations on purpose)" \
        --body "Demo PR for the AIP compliance checker workflow. Violates AIP-127, AIP-131, AIP-132, and AIP-135.")
    PR_NUMBER=$(gh pr view "$PR_URL" --json number -q .number)
    info "PR: $PR_URL"
    sleep "$PAUSE"

    step "Waiting for the workflow to start"
    RUN_ID=""
    for _ in $(seq 1 30); do
        RUN_ID=$(gh run list --workflow="$WORKFLOW_FILE" --branch "$DEMO_BRANCH" \
            --limit 1 --json databaseId -q '.[0].databaseId' 2>/dev/null || true)
        [[ -n "$RUN_ID" ]] && break
        sleep 2
    done
    [[ -n "$RUN_ID" ]] || fail "no workflow run appeared within 60s — check $PR_URL"
    info "run: https://github.com/$(gh repo view --json nameWithOwner -q .nameWithOwner)/actions/runs/$RUN_ID"
    sleep 1

    step "Streaming workflow logs"
    gh run watch "$RUN_ID" --exit-status || info "(workflow finished with a non-zero exit — continuing to fetch the comment)"
    sleep "$PAUSE"

    step "Review comment posted to the PR"
    echo "─────────────────────────────────────────────────────────"
    gh pr view "$PR_NUMBER" --comments \
        | awk '/^## 🤖 AI Code Review/{flag=1} flag' \
        | head -80
    echo "─────────────────────────────────────────────────────────"
    sleep 1

    step "Done"
    info "PR:        $PR_URL"
    info "Cleanup:   gh pr close $PR_NUMBER --delete-branch  &&  git checkout $BASE_BRANCH"
}

main "$@"
