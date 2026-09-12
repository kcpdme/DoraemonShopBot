# Security Policy

## Supported versions

Security fixes are provided for the latest revision on the `main` branch. Older commits and independently modified deployments are not supported.

## Reporting a vulnerability

Use GitHub's private vulnerability-reporting feature when it is enabled for this repository. Include a concise impact description, reproduction steps using test data, and a suggested mitigation if known.

Do not open a public issue containing an unpatched vulnerability, bot token, private key, seed phrase, customer information, wallet address, transaction hash, production log, environment file, or stock payload.

If private reporting is unavailable, open a public issue stating only that you need a private security contact. Do not include exploit details.

## Operator responsibilities

- Rotate any credential that may have been exposed; deleting it from the latest commit is not sufficient.
- Protect the JSON data store because it contains private delivery inventory and customer order records.
- Keep the Mini App behind HTTPS and bind its application listener to a private interface.
- Use trusted RPC providers and monitor payment-verification failures.
- Review dependency, operating-system, and Go security updates before deploying.
