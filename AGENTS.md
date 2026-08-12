# Repository instructions

## Pull requests

- Unless the user explicitly asks otherwise, every task that changes repository files must conclude by opening a pull request for those changes. Create or switch to an appropriate branch if needed, commit only the in-scope changes, push all commits, and open the pull request ready for review.
- When asked to open, create, or publish a pull request, create it as ready for review. Never create a draft pull request.
- Push all committed in-scope changes before opening the pull request.
- If the pull request created or updated during the task is a draft, mark it ready for review after the requested changes are pushed.

## `/lgtm`

- Treat an exact `/lgtm` message as explicit authorization to invoke the repository's `lgtm` skill.
- The authorization covers merging the open pull request for the current branch and deleting that pull request's remote head branch after GitHub confirms the merge.
- After GitHub confirms the pull request was merged, update the repository's main project checkout to the latest `main`: locate the primary checkout with `git worktree list`, switch that checkout to `main` if needed, and pull `main` from its configured remote. Do not discard or overwrite uncommitted changes; report them if they prevent the update.
- Do not ask for an additional merge or branch-deletion confirmation. Do not bypass failing checks, required reviews, merge conflicts, or branch protections; report those blockers instead.
