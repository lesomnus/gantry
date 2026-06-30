#!/usr/bin/env bash

set -o errexit
set -o pipefail

__dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
__root="$(cd "$(dirname "${__dir}")" && pwd)"
__self="${BASH_SOURCE[0]}"

# Current template placeholders
CURRENT_MODULE="github.com/lesomnus/go-app"
CURRENT_ORG_REPO="lesomnus/go-app"
CURRENT_APP_KEBAB="go-app"
CURRENT_APP_UPPER="GO_APP"

usage() {
	echo "Usage: $0 <module-path> [app-name]"
	echo ""
	echo "  module-path  New Go module path (e.g. github.com/my-org/my-app)"
	echo "  app-name     Binary/service name (default: last segment of module-path)"
	echo ""
	echo "Example:"
	echo "  $0 github.com/acme/my-service"
	echo "  $0 github.com/acme/my-service svc"
}

if [ $# -lt 1 ]; then
	usage >&2
	exit 1
fi

NEW_MODULE="$1"
APP_NAME="${2:-${NEW_MODULE##*/}}"

# org/repo from new module path (strip first component, e.g. "github.com/")
NEW_ORG_REPO="${NEW_MODULE#*/}"

# UPPER_CASE variant: replace hyphens with underscores, then uppercase
APP_NAME_UPPER="${APP_NAME//-/_}"
APP_NAME_UPPER="${APP_NAME_UPPER^^}"

echo "Module   : $CURRENT_MODULE  →  $NEW_MODULE"
echo "Org/Repo : $CURRENT_ORG_REPO  →  $NEW_ORG_REPO"
echo "App      : $CURRENT_APP_KEBAB / $CURRENT_APP_UPPER  →  $APP_NAME / $APP_NAME_UPPER"
echo ""

# Find all text source files, excluding this script itself
_find_files() {
	find "$__root" -type f ! -path "$__self" \( \
		-name "*.go"        \
		-o -name "go.mod"   \
		-o -name "*.yml"    \
		-o -name "*.yaml"   \
		-o -name "*.hcl"    \
		-o -name "*.json"   \
		-o -name "Dockerfile"     \
		-o -name "Dockerfile.*"   \
		-o -name ".dockerignore"  \
	\)
}

# 1. Full module path (handles Go imports and https://github.com/... URLs via substring)
_find_files | xargs sed -i "s|${CURRENT_MODULE}|${NEW_MODULE}|g"

# 2. org/repo shorthand remaining after step 1 (ghcr.io image names, devcontainer name, etc.)
_find_files | xargs sed -i "s|${CURRENT_ORG_REPO}|${NEW_ORG_REPO}|g"

# 3. UPPER_CASE env var prefix — before kebab replacement to avoid partial-match issues
_find_files | xargs sed -i "s|${CURRENT_APP_UPPER}|${APP_NAME_UPPER}|g"

# 4. kebab-case binary/service name (Dockerfile paths, root command Name, config file keys)
_find_files | xargs sed -i "s|${CURRENT_APP_KEBAB}|${APP_NAME}|g"

# 5. Rename config files (go-app.yaml / go-app.yml → <app-name>.yaml / <app-name>.yml)
for ext in yaml yml; do
	src="$__root/${CURRENT_APP_KEBAB}.${ext}"
	dst="$__root/${APP_NAME}.${ext}"
	if [ -f "$src" ] && [ "$src" != "$dst" ]; then
		mv "$src" "$dst"
		echo "Renamed: ${CURRENT_APP_KEBAB}.${ext}  →  ${APP_NAME}.${ext}"
	fi
done

echo ""
echo "Done."
echo "Next: cd $__root && go mod tidy"
