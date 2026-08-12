# Repository instructions

## Pull requests

- When asked to open, create, or publish a pull request, create it as ready for review. Never create a draft pull request.
- Push all committed in-scope changes before opening the pull request.
- If the pull request created or updated during the task is a draft, mark it ready for review after the requested changes are pushed.

## `/lgtm`

- Treat an exact `/lgtm` message as explicit authorization to invoke the repository's `lgtm` skill.
- The authorization covers merging the open pull request for the current branch and deleting that pull request's remote head branch after GitHub confirms the merge.
- Do not ask for an additional merge or branch-deletion confirmation. Do not bypass failing checks, required reviews, merge conflicts, or branch protections; report those blockers instead.
