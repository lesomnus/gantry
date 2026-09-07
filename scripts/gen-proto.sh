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
#
# They are built at the commits scripts/orm-tools.lock names, taken out of the
# checkout's object store, so the checkout may sit wherever its other users
# need it and the output here stays the output that is committed. Bumping a
# generator is a deliberate edit of that file; see its header.
#
# Both passes write through a staging area, and the committed protos and pb/
# are put back if any step fails: a half-finished run used to leave the tree
# with pb/ deleted and the service protos already replaced.

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ORM_ROOT="${ORM_ROOT:-/workspaces/github.com/protobuf-orm}"
BUF="${BUF:-go run github.com/bufbuild/buf/cmd/buf@v1.71.0}"

BIN="$ROOT/.gen/bin"
SRC="$ROOT/.gen/src"
SVC="$ROOT/.gen/svc"
MERGED="$ROOT/.gen/merged"
BACKUP="$ROOT/.gen/backup"
LOCK="$ROOT/scripts/orm-tools.lock"

# Restore whatever the run had already replaced. Armed before the first write
# and disarmed on success, so an interrupted run leaves the tree as it found it
# rather than half-generated.
restore() {
	local rc=$?
	if [ "$rc" -eq 0 ]; then
		return 0
	fi
	if [ -d "$BACKUP" ]; then
		echo "gen-proto failed (exit $rc); restoring the committed protos and pb/" >&2
		rm -f "$ROOT"/proto/gantry/*_svc.g.proto
		cp "$BACKUP"/proto/*.proto "$ROOT/proto/gantry/"
		rm -rf "$ROOT/pb"
		cp -r "$BACKUP/pb" "$ROOT/pb"
	fi
	return "$rc"
}

rm -rf "$SRC"
mkdir -p "$BIN"

# Build each tool from the commit the lock file names. The checkout supplies the
# objects and nothing else: its working tree, branch and cleanliness do not
# reach the output.
while read -r repo rev; do
	case "$repo" in '' | '#'*) continue ;; esac

	src="$ORM_ROOT/$repo"
	if [ ! -d "$src" ]; then
		echo "ORM_ROOT=$ORM_ROOT does not contain the $repo repository." >&2
		echo "Clone github.com/protobuf-orm/{protoc-gen-orm-service,protoc-gen-orm-go,protobuf-merge}" >&2
		echo "next to each other and set ORM_ROOT to their parent directory." >&2
		exit 1
	fi
	if ! git -C "$src" cat-file -e "$rev^{commit}" 2>/dev/null; then
		echo "$src does not have commit $rev, which scripts/orm-tools.lock pins $repo to." >&2
		echo "Run: git -C $src fetch" >&2
		exit 1
	fi

	mkdir -p "$SRC/$repo"
	git -C "$src" archive "$rev" | tar -x -C "$SRC/$repo"
	go build -C "$SRC/$repo" -o "$BIN/$repo" .
done <"$LOCK"
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

# Everything that could still fail has passed; from here the tree is written to.
rm -rf "$BACKUP"
mkdir -p "$BACKUP/proto"
cp "$ROOT"/proto/gantry/*_svc.g.proto "$BACKUP/proto/"
cp -r "$ROOT/pb" "$BACKUP/pb"
trap restore EXIT

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

trap - EXIT
rm -rf "$BACKUP"
