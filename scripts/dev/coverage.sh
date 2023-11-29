#!/bin/env bash
echo "Running tests with coverage..."
go test ./... -coverprofile=./coverage.out
