@AGENTS.md

## Claude-specific

- Run tests for the package you touched first (`.claude/skills/scoped-tests`), the
  full suite before a commit.
- When a task adds or changes an endpoint call in `internal/client/`, say in the
  report whether the route exists in the `uppereight/haij` monorepo; you cannot edit
  that repo from here.
- Plans and handovers go in `docs/plans/<YYYY-MM-DD>_<SHORT_NAME>_PLAN.md`. This repo is
  public: keep tenant names, customer data and private haij internals out of them.
