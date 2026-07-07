#!/usr/bin/env bash

set -o errexit
set -o pipefail
# set -o xtrace

# Regenerates the gRPC API from the orm-annotated entity protos:
#
#   pass 1  protoc-gen-orm-service  proto/gantry/*.proto -> .gen/svc/**/*_svc.g.proto
#   merge   protobuf-merge          .gen/svc + proto.svc/gantry -> proto/gantry/*_svc.g.proto
#   pass 2  protoc-gen-go(-grpc), protoc-gen-orm-go -> pb/
#
# The protobuf-orm tools are built from a local checkout of the
# github.com/protobuf-orm repositories (protobuf-merge is not fetchable as a
# Go module). Point ORM_ROOT at the directory that contains them.

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ORM_ROOT="${ORM_ROOT:-/workspaces/github.com/protobuf-orm}"
BUF="${BUF:-go run github.com/bufbuild/buf/cmd/buf@v1.71.0}"

BIN="$ROOT/.gen/bin"
SVC="$ROOT/.gen/svc"
MERGED="$ROOT/.gen/merged"

TOOLS=(protoc-gen-orm-service protoc-gen-orm-go protobuf-merge)

for repo in protobuf-orm "${TOOLS[@]}"; do
	if [ ! -d "$ORM_ROOT/$repo" ]; then
		echo "ORM_ROOT=$ORM_ROOT does not contain the $repo repository." >&2
		echo "Clone github.com/protobuf-orm/{protobuf-orm,protoc-gen-orm-service,protoc-gen-orm-go,protobuf-merge}" >&2
		echo "next to each other and set ORM_ROOT to their parent directory." >&2
		exit 1
	fi
	if [ -n "$(git -C "$ORM_ROOT/$repo" status --porcelain 2>/dev/null)" ]; then
		echo "WARN: $ORM_ROOT/$repo has uncommitted changes;" \
			"the generated output may not be reproducible from a pushed state." >&2
	fi
done

mkdir -p "$BIN"
for tool in "${TOOLS[@]}"; do
	go build -C "$ORM_ROOT/$tool" -o "$BIN/$tool" .
done
export PATH="$BIN:$PATH"

cd "$ROOT"

# Pass 1: entity protos -> CRUD service protos, staged outside the buf module.
rm -rf "$SVC" "$MERGED"
$BUF generate --template buf.gen.svc.yaml

# Merge: overlay hand-written RPCs (List, custom actions) onto the generated
# services. Everything is staged in .gen/merged first; the committed protos
# are replaced only after every merge has succeeded.
mkdir -p "$MERGED"
shopt -s nullglob
generated=("$SVC"/gantry/*_svc.g.proto)
if [ "${#generated[@]}" -eq 0 ]; then
	echo "pass 1 produced no service protos" >&2
	exit 1
fi
for f in "${generated[@]}"; do
	base="$(basename "$f")"
	overlay="$ROOT/proto.svc/gantry/${base%.g.proto}.proto"
	if [ -f "$overlay" ]; then
		"$BIN/protobuf-merge" -o "$MERGED/$base" "$f" "$overlay"
	else
		cp "$f" "$MERGED/$base"
	fi
done

# An overlay without a generated counterpart means an entity was renamed or
# deleted; fail instead of silently dropping the hand-written RPCs.
for overlay in "$ROOT"/proto.svc/gantry/*_svc.proto; do
	base="$(basename "${overlay%.proto}").g.proto"
	if [ ! -f "$MERGED/$base" ]; then
		echo "overlay $overlay has no generated counterpart" >&2
		exit 1
	fi
done

rm -f "$ROOT"/proto/gantry/*_svc.g.proto
cp "$MERGED"/*_svc.g.proto "$ROOT/proto/gantry/"

# Drop stale generated Go so a renamed or deleted entity does not linger;
# hand-written files (e.g. *_test.go) survive.
if [ -d "$ROOT/pb" ]; then
	find "$ROOT/pb" \( -name '*.pb.go' -o -name '*.g.go' \) -delete
fi

# Pass 2: compile everything under proto/gantry into Go stubs.
$BUF generate --template buf.gen.yaml

gofmt -w "$ROOT/pb"
