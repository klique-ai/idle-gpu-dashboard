# Contributing to Klique Idle GPU Dashboard

Thanks for helping make Klique Idle GPU Dashboard better. Bug reports, ideas, documentation fixes and code are all welcome.

## Reporting bugs

Search the [existing issues](../../issues) first. If nobody has reported it yet, [open a new issue](../../issues/new) and include:

- **What happened, and what you expected** to happen instead.
- **Your environment:** Kubernetes, Slurm, PBS or bare metal (with versions), GPU models, NVIDIA driver version, GPU Operator version if you use it, and the Klique Idle GPU Dashboard version.
- **Steps to reproduce**, if you can.
- **Relevant logs** from the collector or the hub, and the output of `klique-idle-collector --once` if you can. Remove API keys, and any hostnames, job names or user names you don't want to share, before posting.

## Suggesting features

Open an issue that describes the problem you're trying to solve, not only the solution you have in mind. Knowing how you run your cluster helps us judge what will be useful to others too.

## Reporting security issues

Don't report security vulnerabilities in a public issue. Use GitHub's [private vulnerability reporting](../../security/advisories/new) instead, so we can fix the problem before it's made public.

## Contributing code and documentation

1. **Open an issue first** for anything larger than a small fix, so we can agree on the approach before you invest time in it.
2. **Fork the repository** and create a branch for your change. The [Development](README.md#development) section of the README explains how to build and test.
3. **Keep each pull request focused** on one change.
4. **Describe what it does and why** in the pull request, and link the related issue.
5. **Test your change** and say how you tested it: which environment, which GPUs.
6. **Update the documentation** if your change affects how people install, configure or use the project.

A maintainer will review your pull request and may ask for changes before merging it.

## Using AI tools

You're welcome to use AI tools when contributing, as long as a person stands behind the result:

- **You're the author.** You're responsible for everything you submit. Review, test and understand it, and be ready to explain any part of it in review.
- **Treat AI output as a draft.** Check generated code, tests and descriptions as carefully as you would a colleague's first draft.
- **Make it worth reviewing.** A contribution should be worth more to the project than the time it takes to review it.

Low-effort, unreviewed AI-generated pull requests and issues will be closed without review.

## Licensing of contributions

Klique Idle GPU Dashboard is Copyright 2026 ClearML Ltd. and is licensed under the [Apache License 2.0](LICENSE). By submitting a contribution, you agree that it is licensed under the same terms, as described in Section 5 of the license, and that you have the right to submit it.
