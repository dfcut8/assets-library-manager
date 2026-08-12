---
name: lgtm
description: Merge the current branch's open GitHub pull request and delete its remote head branch after a verified merge. Use when the user invokes `/lgtm` or explicitly invokes this skill to merge the active pull request. Do not use for review-only requests or casual approval language.
---

# LGTM

Merge the single open pull request associated with the current branch, then delete its remote head branch. Invocation is explicit authorization for those two external changes only.

## Workflow

1. Inspect the local repository before changing GitHub state.
   - Require a clean working tree.
   - Resolve the repository, current branch, upstream, and remote default branch.
   - Require the local HEAD to be pushed to the pull request's remote head branch.
   - Never operate on a detached HEAD, the default branch, or a branch belonging to a different pull request.
2. Resolve exactly one open pull request whose head matches the current branch.
   - Stop and report if none or more than one matches.
   - Confirm the base branch and repository before merging.
3. Inspect the pull request immediately before merge.
   - Mark it ready for review if it is still a draft.
   - Require GitHub to report it mergeable with no conflicts.
   - Require all required checks and reviews to pass.
   - Stop on pending or failing required checks, requested changes, merge conflicts, merge queues, or branch-protection blockers. Never bypass protections or use administrator override.
4. Merge the pull request.
   - Prefer squash merge; if the repository disables squash, use rebase, then merge commit, in that order.
   - Request deletion of the remote head branch as part of the merge operation.
5. Verify the result through GitHub.
   - Require the pull request state to be `MERGED` and capture its merge commit and merge time.
   - Only after that verification, check whether the remote head ref still exists.
   - If it remains and is not a protected/default branch, delete it explicitly.
6. Prune remote-tracking references and report the pull request URL, merge method, merge commit, and branch-deletion result.

## Safety boundaries

- `/lgtm` does not authorize merging any pull request other than the current branch's unique match.
- Do not merge unpushed or uncommitted work.
- Do not delete a branch before GitHub confirms the pull request was merged.
- Always delete the merged pull request's remote head branch unless GitHub identifies it as the default or a protected branch. If permissions prevent deletion, report the failure explicitly.
- Do not delete unrelated local branches. Local branch cleanup is optional and must not discard work or disrupt another worktree.
