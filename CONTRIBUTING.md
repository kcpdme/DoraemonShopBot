# Contributing

Thank you for helping improve Doraemon Shop Bot.

## Before opening an issue

- Search existing issues first.
- Use the bug-report template for reproducible defects.
- Never post bot tokens, private keys, seed phrases, customer information, transaction details, wallet addresses, production logs, or stock payloads.
- Use GitHub's private vulnerability-reporting feature for security-sensitive reports when it is available. Do not disclose exploitable details in a public issue.

## Development workflow

1. Fork the repository and create a focused branch.
2. Keep credentials in an ignored `.env` file. Use fabricated values in tests and documentation.
3. Add or update tests for behavioral changes.
4. Run the required checks:

   ```bash
   go test -race ./...
   go vet ./...
   ```

5. Open a pull request describing the problem, the solution, and how it was verified.

Avoid unrelated formatting or refactoring in the same pull request. New dependencies should include a clear reason and should be kept to a minimum.

## Pull-request expectations

- Preserve private-chat-only handling for payments, stock, and support.
- Never expose stock payloads, buyer identities, or operational configuration through Mini App APIs or public announcements.
- Validate Telegram Mini App identity server-side.
- Keep inventory reservation and order-status changes atomic.
- Document new environment variables in both `.env.example` and `README.md`.
- Maintain backward compatibility for bot-only deployments unless a breaking change is explicitly proposed.

By contributing, you agree that your contribution is licensed under the repository's MIT License.
