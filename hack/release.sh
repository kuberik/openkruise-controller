#!/usr/bin/env bash

set -euo pipefail

# Get the current branch
BRANCH=$(git rev-parse --abbrev-ref HEAD)

# Get the latest tag
LATEST_TAG=$(git describe --tags --abbrev=0 2>/dev/null || echo "v0.0.0")

# Split version into components
IFS='.' read -r -a VERSION_PARTS <<< "${LATEST_TAG#v}"
MAJOR="${VERSION_PARTS[0]}"
MINOR="${VERSION_PARTS[1]}"
PATCH="${VERSION_PARTS[2]}"

# Determine version increment based on branch
if [[ "$BRANCH" == "main" ]]; then
    # On main branch, increment minor version
    NEW_VERSION="v${MAJOR}.$((MINOR + 1)).0"
    RELEASE_BRANCH="release-${MAJOR}.$((MINOR + 1))"
elif [[ "$BRANCH" =~ ^release- ]]; then
    # On release branch, increment patch version
    NEW_VERSION="v${MAJOR}.${MINOR}.$((PATCH + 1))"
else
    echo "Error: Must be on main or release-* branch to create a release"
    exit 1
fi

echo "Current version: ${LATEST_TAG}"
echo "New version: ${NEW_VERSION}"

(
    cd config/manager
    kustomize edit set image controller=ghcr.io/kuberik/openkruise-controller:${NEW_VERSION}
)

# Generate changelog
echo "Generating changelog..."
make changelog VERSION="${NEW_VERSION}"

# Create git tag
git add CHANGELOG.md config/manager/kustomization.yaml
git commit -m "chore: release version ${NEW_VERSION}"
git tag -a "${NEW_VERSION}" -m "Release ${NEW_VERSION}"

# For minor releases on main, create a release branch
if [[ "$BRANCH" == "main" ]]; then
    git checkout -b "${RELEASE_BRANCH}"
    echo "Created release branch: ${RELEASE_BRANCH}"
fi

echo "Created release ${NEW_VERSION}"
echo "To push the changes, run:"
if [[ "$BRANCH" == "main" ]]; then
    echo "git push origin main"
    echo "git push origin ${RELEASE_BRANCH}"
fi
echo "git push origin ${NEW_VERSION}"
