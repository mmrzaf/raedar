# Contributing to Raedar

Thanks for your interest. Raedar is deliberately small and focused on one kind of server: Docker workloads behind Traefik. Contributions that make it better at that are very welcome.

## Before you start

- Read the [overview](docs/overview.md) and [design decisions](docs/decisions.md). Several features are out of scope on purpose (actions, log storage, custom dashboards, built-in authentication, other platforms).
- For anything beyond a small fix, open an issue first and describe the problem you want to solve. It saves both sides time.

## Making a change

1. Fork the repository and create a branch.
2. Follow [development.md](docs/development.md) to run Raedar locally and run the tests.
3. Keep changes focused. Add or update tests for behavior you change.
4. Update the documentation in `docs/` when behavior, configuration, labels or the webhook payload change.
5. Make sure `gofmt`, `go vet`, `staticcheck` and the tests pass.
6. Open a pull request that explains what changes and why.

## Releases

`VERSION` holds the current version. A release is a `v<VERSION>` tag on a commit of `main` that passed CI; automation validates and publishes tags but never creates or moves them.

1. Bump `VERSION` on `develop` (a patch for fixes) and merge it to `main`.
2. Wait for CI on `main` to pass.
3. Create an annotated tag `v<VERSION>` and push it to both remotes.

The tag makes Gitman build and deploy `raedar:<VERSION>` on the production host; GitHub independently publishes the multi-arch image and the release. Database migrations are forward-only; back up the database before a release that changes the schema.

## Reporting bugs

Include the Raedar version, Docker and Traefik versions, the relevant configuration (with secrets removed), the output of `raedar check`, and what you expected to happen.

## Security issues

Do not open public issues for vulnerabilities. See [SECURITY.md](SECURITY.md).

## License

By contributing, you agree that your contributions are licensed under the [MIT License](LICENSE).
