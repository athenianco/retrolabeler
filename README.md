# retrolabeler

The tool to label existing PRs in a GitHub repository according to the rules in [actions/labeler](https://github.com/actions/labeler) format.

**actions/labeler** is a GitHub Action that defines the YAML configuration format of the rules how to label pull requests according to which files were changed.
It works great for new PRs, but there is no way currently to label already existing ones. **retrolabeler** solves that problem.
The tool bases on v4 GraphQL API and has these features:
- Automatic retries on 5xx responses (GitHub backend crashes).
- Automatic sleep upon draining the rate limit.
- Create the labels mentioned in YAML but not present in the repository on the fly (`-c`).
- Specify the date since which to label PRs (`-s`).
- Dry run mode: execute everything but the actual mutations - label creation and PR labeling (`-dry-run`).
- Fast PR labeling in multiple parallel workers (`-j`, 2 by default).

## Installation

You need to have a [Go compiler](https://go.dev/dl/) 1.17+.

```
GOBIN=$(pwd) go install github.com/athenianco/retrolabeler@latest
```

## Usage

Obtain the GitHub token either from your [Personal Access Tokens](https://docs.github.com/en/authentication/keeping-your-account-and-data-secure/creating-a-personal-access-token)
or by running the provided Python script intended for GitHub application developers `cat app_private_key.pem | python3 token_from_pem.py <installation id>`.

```
export GITHUB_TOKEN=...
cat .github/labeler.yml | retrolabeler -c owner/reponame
```

Note: the tool does not support all the advanced features of [minimatch](https://github.com/isaacs/minimatch).
The underlying glob engine is [gobwas/glob](https://github.com/gobwas/glob#performance).

## License

Apache 2.0, see [LICENSE](LICENSE).