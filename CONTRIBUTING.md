# Contributing to CrossCodex

## Prerequisites

- **Go** >= 1.26
- **Task** ([taskfile.dev](https://taskfile.dev)) - build automation
- **Buf** ([buf.build](https://buf.build)) - protobuf tooling
- **Podman or Docker** - required for integration tests

## Getting Started

1. Fork the repository and clone your fork:
   ```bash
   git clone git@github.com:<your-user>/crosscodex.git
   cd crosscodex
   ```

2. Install dependencies:
   ```bash
   task dev:deps
   ```

## Development Workflow

1. Open an issue first for large features or architectural changes.
2. Create a feature branch from `main`:
   ```bash
   git checkout -b feature/123-short-description main
   ```
3. Write tests first (TDD): test, see it fail, implement, see it pass.
4. Run the full check suite before submitting:
   ```bash
   task check
   ```

## Commit Messages

Use imperative mood, max 72 characters for the subject line.

```
feat(config): add profile selection
fix(db): close idle connections on shutdown
test(natsbus): add TLS integration coverage
docs(readme): update build instructions
```

## Pull Requests

- Reference the related issue (e.g., "Closes #123").
- Describe **what** changed and **why**.
- Keep PRs focused -- one logical change per PR.
- All CI checks must pass before review.

## Testing

- **Unit tests** are required for all changes: `task test:unit`
- **Integration tests** are required for infrastructure changes: `task test:integration:<name>`
- Follow table-driven test patterns used in the existing codebase.
- Never commit test fixtures that contain credentials or secrets.

## Code Style

- Follow existing patterns in the codebase. Read surrounding code before editing.
- Run `task lint` and fix any issues before submitting.
- Do not introduce new linters or formatters without discussion.

## License

By contributing, you agree that your contributions will be licensed under the [Apache License 2.0](LICENSE).
