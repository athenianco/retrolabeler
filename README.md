# retrolabeler

The tool to label existing PRs in a GitHub repository according to the rules in [actions/labeler](https://github.com/actions/labeler) format.

## Installation

```
GOBIN=$(pwd) go install github.com/athenianco/retrolabeler@latest
```

## Usage

```
export GITHUB_TOKEN=...
cat .github/labeler.yml | retrolabeler -c owner/reponame
```

## License

Apache 2.0, see [LICENSE](LICENSE).