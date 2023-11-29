#!/usr/bin/env bash
echo "Installing locally"

go build -o codefly main.go
mv codefly ~/go/bin/
codefly completion zsh > ~/.oh-my-zsh/completions/_codefly
