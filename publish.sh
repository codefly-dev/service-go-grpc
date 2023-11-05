VERSION=$(cat plugin.codefly.yaml| yq e '.version')
git tag -a "$VERSION" -m "Release $VERSION"
GOPRIVATE=github.com/codefly-dev/cli goreleaser release
