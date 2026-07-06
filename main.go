package main

import (
	"context"
	"fmt"
	"os"

	"github.com/lesomnus/gantry/cmd"
)

//go:generate go run github.com/swaggo/swag/v2/cmd/swag@v2.0.0-rc5 init -g main.go -d ./,internal/server,internal/warm,internal/store,internal/down,internal/retention,cmd/config --parseInternal --v3.1 --ot json,yaml -o internal/server/oapi
//go:generate python3 scripts/fix-openapi.py
//go:generate bash scripts/gen-api-docs.sh

//	@title			gantry API
//	@version		1.0
//	@description	Move container images between stores (OCI registries and docker/containerd engines) and track per-layer progress.
//	@description	A job moves an image `from` an OCI store into `to` — an OCI registry (gantry copies the blobs) or a docker/containerd engine (the daemon pulls it).

//	@securityDefinitions.apikey	BearerAuth
//	@in							header
//	@name						Authorization

func main() {
	c := cmd.NewCmdRoot()
	if err := c.Run(context.Background(), os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
