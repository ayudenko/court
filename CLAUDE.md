@AGENTS.md

# Claude Code orchestration

Use the project subagents in `.claude/agents/` as follows:

- Run `code-reviewer` after code, test, build, CI, or configuration changes and
  resolve every blocking finding before completion.
- Also run `security-reviewer` when a change touches auth, credentials, untrusted
  input, permissions, public endpoints, moderator spending, or another trust
  boundary.
- Run `adversarial-reviewer` before accepting an ADR-triggering or
  expensive-to-reverse design decision. Give it only the specification, diff,
  and ADR—not the author's reasoning transcript.
- Use multiple `researcher` agents in parallel only for independent read-only
  investigations. The main conversation synthesizes their evidence and owns
  all edits.

When delegating review, include the task specification, current status, complete
diff, relevant ADRs, and validation output in the task. Reviewers intentionally
have no shell tool: they must not mutate the worktree, Git state, or network.

Before planning a change covered by an ADR trigger in `AGENTS.md`, read
`docs/adr/0001-risk-based-development.md` and any newer relevant ADRs.

Custom reviewers are read-only. Do not ask them to fix their findings; make the
fix in the main conversation and rerun the relevant reviewer. Built-in Explore
and Plan agents do not load project instructions, so restate any critical
constraint in their task prompt if you use them.

Before completion, run `make check` and report the review agents used, their
findings, and the validation result.
