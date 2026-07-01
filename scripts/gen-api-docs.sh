#!/usr/bin/env bash

set -o errexit
set -o pipefail
# set -o xtrace

__dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)" # Directory where this script exists.
__root="$(cd "$(dirname "${__dir}")" && pwd)"         # Root directory of project.

# Render the OpenAPI 3.1 spec into a human-readable Markdown reference using
# widdershins (https://github.com/Mermade/widdershins), the community-standard
# OpenAPI -> Markdown converter. The spec is the source of truth: regenerate it
# with swag first (see the //go:generate in main.go), then run this.
spec="${__root}/internal/server/oapi/swagger.json"
out="${__root}/docs/api.md"

if [ ! -f "$spec" ]; then
	echo "spec not found: ${spec} (run 'go generate ./...' to build it first)" >&2
	exit 1
fi

tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT

# --omitHeader drops the Slate-specific YAML front matter; --language_tabs keeps
# a single curl sample per operation; --summary uses the operation summaries as
# section titles.
npx -y widdershins@4.0.1 \
	--search false \
	--language_tabs 'shell:curl' \
	--summary \
	--omitHeader \
	"$spec" -o "$tmp"

{
	echo "<!-- Generated from internal/server/oapi/swagger.json by scripts/gen-api-docs.sh — do not edit by hand. -->"
	echo
	cat "$tmp"
} >"$out"

echo "wrote ${out}"
