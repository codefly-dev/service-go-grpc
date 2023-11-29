#!/bin/bash

# Usage: ./update_version.sh <new_version>

NEW_VERSION=$1
YAML_FILE="agent.codefly.yaml"

if [ -z "$NEW_VERSION" ]; then
    echo "Error: No version provided."
    exit 1
fi

if [ ! -f "$YAML_FILE" ]; then
    echo "Error: YAML file $YAML_FILE does not exist."
    exit 1
fi

# Update the version in the YAML file (for macOS)
sed -i '' "s/version:.*/version: $NEW_VERSION/" "$YAML_FILE"

# Add the changes to git
git add "$YAML_FILE"

# Commit the change
git commit -m "Update version to $NEW_VERSION"

# Tag the commit
git tag -a "v$NEW_VERSION" -m "Version $NEW_VERSION" -f

# Force push the commit and tag to the remote repository
git push -f
git push origin "v$NEW_VERSION" -f
