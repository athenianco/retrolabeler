# retrolabeler

The tool to label existing PRs in a GitHub repository according to the rules in [actions/labeler](https://github.com/actions/labeler) format.

## Installation

```
go get github.com/athenianco/retrolabeler/cmd/retrolabeler
```

## Usage

```
export GITHUB_TOKEN=...
cat .github/labeler.yml | retrolabeler owner/reponame
```

## License

Apache 2.0, see [LICENSE](LICENSE).