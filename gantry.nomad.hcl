# Nomad job for the gantry server.
#
#   nomad job run gantry.nomad.hcl
#
# The gRPC API listens on static port 8080 (grpcurl works out of the box —
# reflection is public). The gantry config is rendered from the template
# below into the task directory; edit the `stores` block to match your
# environment (it mirrors gantry.hday.yaml).

variable "image" {
  type = string
  # `edge` tracks the latest build; pin a dated tag (e.g. "260707") for
  # reproducible deploys.
  default = "ghcr.io/lesomnus/gantry:edge"
}

variable "grpc_port" {
  type    = number
  default = 8080
}

job "gantry" {
  datacenters = ["*"]
  type        = "service"

  group "gantry" {
    count = 1

    # The dockerd-local store drives the docker daemon of the node this
    # allocation lands on. Pin the job to a specific node if that matters:
    # constraint {
    #   attribute = "${attr.unique.hostname}"
    #   value     = "node-10-33"
    # }

    network {
      port "grpc" {
        static = var.grpc_port
        to     = var.grpc_port
      }
    }

    service {
      name     = "gantry"
      port     = "grpc"
      provider = "nomad"

      check {
        type     = "tcp"
        interval = "10s"
        timeout  = "3s"
      }

      # With Consul instead, use the real health service (gantry serves
      # grpc.health.v1.Health, exempt from auth; readiness follows the
      # gated stores — serve.health.ready_stores):
      # provider = "consul"
      # check {
      #   type         = "grpc"
      #   interval     = "10s"
      #   timeout      = "3s"
      #   grpc_service = ""
      # }
    }

    task "server" {
      driver = "docker"

      config {
        image = var.image
        ports = ["grpc"]
        # The --config flag must precede the subcommand (it is a persistent
        # root flag), so it comes before "serve".
        args  = ["--config", "${NOMAD_TASK_DIR}/gantry.yaml", "serve"]

        # For the unix-socket docker store below. Requires the client's
        # docker plugin to allow it (plugin "docker" { config { volumes {
        # enabled = true } } }); drop both when every engine store is tcp.
        volumes = ["/var/run/docker.sock:/var/run/docker.sock"]
        # The image runs as 65532; the docker socket is usually root:docker.
        # Either add 65532 to the docker group on the host or run as root:
        # user = "0"
      }

      template {
        destination = "local/gantry.yaml"
        data        = <<-EOT
          serve:
            addr: ":${var.grpc_port}"
            # allow_unknown_stores: true      # let jobs name bare registry hosts
            # auth:
            #   tokens: ["$${GANTRY_TOKEN}"]  # env-expanded by gantry
            # events:
            #   path: "/data/events.db"       # needs a persistent volume

          stores:
            # Source registry (remote). Jobs name this as `source`.
            remote:
              kind: oci
              host: cr.hday.io

            # Local cache registry gantry fills and the daemons pull from.
            # Jobs name this as `target`.
            local:
              kind: oci
              host: "192.168.10.33:5000"
              insecure: true        # plain-HTTP registry on :5000
              mode: copy            # push blobs into the cache
              # In-store ref = source repo/tag under this store's host.

            # Docker daemon of the node this allocation runs on.
            dockerd-local:
              kind: docker
              address: "unix:///var/run/docker.sock"

            # Remote daemon over plain tcp.
            dockerd-10-34:
              kind: docker
              address: "tcp://192.168.10.34:2375"

            # TPM-backed mTLS daemon (see gantry.hday.yaml): needs the TPM
            # device and the cert files mounted into the task first.
            # dockerd-10-2:
            #   kind: docker
            #   address: "tcp://192.168.10.2:2376"
            #   cred:
            #     kind: tpm
            #     handle: "0x81666479"
            #     cert: "device.crt"
            #   ca_cert: "ca.crt"
        EOT
      }

      resources {
        cpu    = 500
        memory = 256
        # Registry copies stream blob-by-blob; memory stays flat even for
        # large images, so bump this only if many concurrent jobs run.
      }
    }
  }
}
