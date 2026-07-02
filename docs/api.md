<!-- Generated from internal/server/oapi/swagger.json by scripts/gen-api-docs.sh — do not edit by hand. -->

<!-- Generator: Widdershins v4.0.1 -->

<h1 id="gantry-api">gantry API v1.0</h1>

Move container images between stores (OCI registries and docker/containerd engines) and track per-layer progress.
A job copies an image `from` an OCI store `to` another, then `distribute` engines pull it.

# Authentication

* API Key (BearerAuth)
    - Parameter Name: **Authorization**, in: header. 

<h1 id="gantry-api-meta">meta</h1>

## Liveness probe

> Code samples

```shell
# You can also use wget
curl -X GET /healthz \
  -H 'Accept: text/plain'

```

`GET /healthz`

> Example responses

> 200 Response

```
"string"
```

<h3 id="liveness-probe-responses">Responses</h3>

|Status|Meaning|Description|Schema|
|---|---|---|---|
|200|[OK](https://tools.ietf.org/html/rfc7231#section-6.3.1)|ok|string|

<aside class="success">
This operation does not require authentication
</aside>

## Readiness probe

> Code samples

```shell
# You can also use wget
curl -X GET /readyz \
  -H 'Accept: application/json'

```

`GET /readyz`

Aggregate readiness over the gated stores (serve.health.ready_stores; defaults to every engine store, so a flaky remote upstream cannot flap the node). 200 when all gated stores are healthy, 503 otherwise. Unauthenticated like /healthz, but the per-store breakdown is included only for authenticated callers.

> Example responses

> 200 Response

```json
{
  "ready": true,
  "stores": [
    {
      "cached": true,
      "checked_at": "string",
      "error": "string",
      "healthy": true,
      "kind": "string",
      "latency_ms": 0,
      "name": "string"
    }
  ]
}
```

<h3 id="readiness-probe-responses">Responses</h3>

|Status|Meaning|Description|Schema|
|---|---|---|---|
|200|[OK](https://tools.ietf.org/html/rfc7231#section-6.3.1)|OK|[server.readyzResponse](#schemaserver.readyzresponse)|
|503|[Service Unavailable](https://tools.ietf.org/html/rfc7231#section-6.6.4)|Service Unavailable|[server.readyzResponse](#schemaserver.readyzresponse)|

<aside class="success">
This operation does not require authentication
</aside>

## Build info

> Code samples

```shell
# You can also use wget
curl -X GET /v1/version \
  -H 'Accept: application/json' \
  -H 'Authorization: API_KEY'

```

`GET /v1/version`

The running binary's version stamp, for remote fleet inventory during rolling upgrades.

> Example responses

> 200 Response

```json
{
  "git_dirty": true,
  "git_rev": "string",
  "version": "string"
}
```

<h3 id="build-info-responses">Responses</h3>

|Status|Meaning|Description|Schema|
|---|---|---|---|
|200|[OK](https://tools.ietf.org/html/rfc7231#section-6.3.1)|OK|[server.versionResponse](#schemaserver.versionresponse)|

<aside class="warning">
To perform this operation, you must be authenticated by means of one of the following methods:
BearerAuth
</aside>

<h1 id="gantry-api-retention">retention</h1>

## GC scheduler status

> Code samples

```shell
# You can also use wget
curl -X GET /v1/gc \
  -H 'Accept: application/json' \
  -H 'Authorization: API_KEY'

```

`GET /v1/gc`

Scheduler observability: when GC last ran and will next wake, whether the post-startup grace window is holding age deletion off, the effective default policy, and per-engine index sizes. Never probes live daemons.

> Example responses

> 200 Response

```json
{
  "enabled": true,
  "grace_until": "string",
  "last_run": "string",
  "next_wake": "string",
  "policy": {
    "keep_n": 0,
    "max_age": "string",
    "pins": [
      "string"
    ]
  },
  "running": true,
  "schedule": {
    "grace": "string",
    "interval": "string",
    "min_interval": "string"
  },
  "started": "string",
  "stores": {
    "property1": {
      "pins": 0,
      "records": 0
    },
    "property2": {
      "pins": 0,
      "records": 0
    }
  }
}
```

<h3 id="gc-scheduler-status-responses">Responses</h3>

|Status|Meaning|Description|Schema|
|---|---|---|---|
|200|[OK](https://tools.ietf.org/html/rfc7231#section-6.3.1)|OK|[retention.Status](#schemaretention.status)|
|501|[Not Implemented](https://tools.ietf.org/html/rfc7231#section-6.6.2)|Not Implemented|[server.errorResponse](#schemaserver.errorresponse)|

<aside class="warning">
To perform this operation, you must be authenticated by means of one of the following methods:
BearerAuth
</aside>

## Evaluate retention GC for a store (dry-run)

> Code samples

```shell
# You can also use wget
curl -X GET /v1/store/{name}/gc \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json' \
  -H 'Authorization: API_KEY'

```

`GET /v1/store/{name}/gc`

Returns the retention decision (keep/delete) without deleting anything. An optional body overrides the configured max_age/keep_n/pins for this call. NOTE: a GET request body is dropped by fetch()/XHR, some HTTP clients, and proxies — to pass overrides reliably, use POST.

> Body parameter

```json
{
  "keep_n": 3,
  "max_age": "720h",
  "pins": [
    "docker.io/library/nginx:1.27"
  ]
}
```

<h3 id="evaluate-retention-gc-for-a-store-(dry-run)-parameters">Parameters</h3>

|Name|In|Type|Required|Description|
|---|---|---|---|---|
|name|path|string|true|engine store name|
|body|body|[server.gcRequest](#schemaserver.gcrequest)|false|policy overrides|

> Example responses

> 200 Response

```json
{
  "delete": [
    {
      "digest": "string",
      "last_used": "string",
      "reason": "string",
      "ref": "string"
    }
  ],
  "keep": [
    {
      "reason": "string",
      "ref": "string"
    }
  ],
  "next_age_out": "string"
}
```

<h3 id="evaluate-retention-gc-for-a-store-(dry-run)-responses">Responses</h3>

|Status|Meaning|Description|Schema|
|---|---|---|---|
|200|[OK](https://tools.ietf.org/html/rfc7231#section-6.3.1)|dry-run keep/delete decision|[retention.Decision](#schemaretention.decision)|
|400|[Bad Request](https://tools.ietf.org/html/rfc7231#section-6.5.1)|Bad Request|[server.errorResponse](#schemaserver.errorresponse)|
|404|[Not Found](https://tools.ietf.org/html/rfc7231#section-6.5.4)|Not Found|[server.errorResponse](#schemaserver.errorresponse)|
|501|[Not Implemented](https://tools.ietf.org/html/rfc7231#section-6.6.2)|Not Implemented|[server.errorResponse](#schemaserver.errorresponse)|
|502|[Bad Gateway](https://tools.ietf.org/html/rfc7231#section-6.6.3)|Bad Gateway|[server.errorResponse](#schemaserver.errorresponse)|

<aside class="warning">
To perform this operation, you must be authenticated by means of one of the following methods:
BearerAuth
</aside>

## Run retention GC for a store (apply)

> Code samples

```shell
# You can also use wget
curl -X POST /v1/store/{name}/gc \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json' \
  -H 'Authorization: API_KEY'

```

`POST /v1/store/{name}/gc`

Applies the deletions and returns the apply result. An optional body overrides the configured max_age/keep_n/pins for this call.

> Body parameter

```json
{
  "keep_n": 3,
  "max_age": "720h",
  "pins": [
    "docker.io/library/nginx:1.27"
  ]
}
```

<h3 id="run-retention-gc-for-a-store-(apply)-parameters">Parameters</h3>

|Name|In|Type|Required|Description|
|---|---|---|---|---|
|name|path|string|true|engine store name|
|body|body|[server.gcRequest](#schemaserver.gcrequest)|false|policy overrides|

> Example responses

> 200 Response

```json
{
  "deleted": [
    "string"
  ],
  "errors": [
    "string"
  ],
  "evaluated": 0,
  "untagged": [
    "string"
  ]
}
```

<h3 id="run-retention-gc-for-a-store-(apply)-responses">Responses</h3>

|Status|Meaning|Description|Schema|
|---|---|---|---|
|200|[OK](https://tools.ietf.org/html/rfc7231#section-6.3.1)|deletions applied|[retention.ApplyResult](#schemaretention.applyresult)|
|400|[Bad Request](https://tools.ietf.org/html/rfc7231#section-6.5.1)|Bad Request|[server.errorResponse](#schemaserver.errorresponse)|
|404|[Not Found](https://tools.ietf.org/html/rfc7231#section-6.5.4)|Not Found|[server.errorResponse](#schemaserver.errorresponse)|
|501|[Not Implemented](https://tools.ietf.org/html/rfc7231#section-6.6.2)|Not Implemented|[server.errorResponse](#schemaserver.errorresponse)|
|502|[Bad Gateway](https://tools.ietf.org/html/rfc7231#section-6.6.3)|Bad Gateway|[server.errorResponse](#schemaserver.errorresponse)|

<aside class="warning">
To perform this operation, you must be authenticated by means of one of the following methods:
BearerAuth
</aside>

## Delete a retention index record

> Code samples

```shell
# You can also use wget
curl -X DELETE /v1/store/{name}/image \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json' \
  -H 'Authorization: API_KEY'

```

`DELETE /v1/store/{name}/image`

Purges one record from the retention index WITHOUT touching the engine — the escape hatch for orphan records left by out-of-band image removal. A record for an image still present is re-created (with fresh timestamps) by the usage watcher or the next distribute. To delete the image itself use /remove.

> Body parameter

```json
{
  "ref": "docker.io/library/nginx:1.27"
}
```

<h3 id="delete-a-retention-index-record-parameters">Parameters</h3>

|Name|In|Type|Required|Description|
|---|---|---|---|---|
|name|path|string|true|engine store name|
|body|body|[server.removeRequest](#schemaserver.removerequest)|true|reference whose record to purge|

> Example responses

> 400 Response

```json
{
  "error": "string"
}
```

<h3 id="delete-a-retention-index-record-responses">Responses</h3>

|Status|Meaning|Description|Schema|
|---|---|---|---|
|204|[No Content](https://tools.ietf.org/html/rfc7231#section-6.3.5)|No Content|None|
|400|[Bad Request](https://tools.ietf.org/html/rfc7231#section-6.5.1)|Bad Request|[server.errorResponse](#schemaserver.errorresponse)|
|404|[Not Found](https://tools.ietf.org/html/rfc7231#section-6.5.4)|Not Found|[server.errorResponse](#schemaserver.errorresponse)|
|500|[Internal Server Error](https://tools.ietf.org/html/rfc7231#section-6.6.1)|Internal Server Error|[server.errorResponse](#schemaserver.errorresponse)|
|501|[Not Implemented](https://tools.ietf.org/html/rfc7231#section-6.6.2)|Not Implemented|[server.errorResponse](#schemaserver.errorresponse)|

<aside class="warning">
To perform this operation, you must be authenticated by means of one of the following methods:
BearerAuth
</aside>

## List a store's image inventory

> Code samples

```shell
# You can also use wget
curl -X GET /v1/store/{name}/image \
  -H 'Accept: application/json' \
  -H 'Authorization: API_KEY'

```

`GET /v1/store/{name}/image`

The retention index records for an engine store — what gantry believes is on the node and the timestamps driving GC decisions (last_used, last_distributed, first_seen, pinned).

<h3 id="list-a-store's-image-inventory-parameters">Parameters</h3>

|Name|In|Type|Required|Description|
|---|---|---|---|---|
|name|path|string|true|engine store name|
|repo|query|string|false|filter by exact repository|
|ref|query|string|false|return only this exact reference|
|pinned|query|boolean|false|filter by pinned state|

> Example responses

> 200 Response

```json
{
  "items": [
    {
      "digest": "string",
      "first_seen": "string",
      "last_distributed": "string",
      "last_used": "string",
      "pinned": true,
      "ref": "string",
      "repo": "string",
      "tag": "string"
    }
  ]
}
```

<h3 id="list-a-store's-image-inventory-responses">Responses</h3>

|Status|Meaning|Description|Schema|
|---|---|---|---|
|200|[OK](https://tools.ietf.org/html/rfc7231#section-6.3.1)|OK|[server.imageListResponse](#schemaserver.imagelistresponse)|
|400|[Bad Request](https://tools.ietf.org/html/rfc7231#section-6.5.1)|Bad Request|[server.errorResponse](#schemaserver.errorresponse)|
|404|[Not Found](https://tools.ietf.org/html/rfc7231#section-6.5.4)|Not Found|[server.errorResponse](#schemaserver.errorresponse)|
|500|[Internal Server Error](https://tools.ietf.org/html/rfc7231#section-6.6.1)|Internal Server Error|[server.errorResponse](#schemaserver.errorresponse)|
|501|[Not Implemented](https://tools.ietf.org/html/rfc7231#section-6.6.2)|Not Implemented|[server.errorResponse](#schemaserver.errorresponse)|

<aside class="warning">
To perform this operation, you must be authenticated by means of one of the following methods:
BearerAuth
</aside>

## Unpin a reference or pattern

> Code samples

```shell
# You can also use wget
curl -X DELETE /v1/store/{name}/pin \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json' \
  -H 'Authorization: API_KEY'

```

`DELETE /v1/store/{name}/pin`

> Body parameter

```json
{
  "pattern": "*:stable",
  "ref": "docker.io/library/nginx:1.27"
}
```

<h3 id="unpin-a-reference-or-pattern-parameters">Parameters</h3>

|Name|In|Type|Required|Description|
|---|---|---|---|---|
|name|path|string|true|engine store name|
|body|body|[server.pinRequest](#schemaserver.pinrequest)|true|reference or pattern to unpin|

> Example responses

> 400 Response

```json
{
  "error": "string"
}
```

<h3 id="unpin-a-reference-or-pattern-responses">Responses</h3>

|Status|Meaning|Description|Schema|
|---|---|---|---|
|204|[No Content](https://tools.ietf.org/html/rfc7231#section-6.3.5)|No Content|None|
|400|[Bad Request](https://tools.ietf.org/html/rfc7231#section-6.5.1)|Bad Request|[server.errorResponse](#schemaserver.errorresponse)|
|404|[Not Found](https://tools.ietf.org/html/rfc7231#section-6.5.4)|Not Found|[server.errorResponse](#schemaserver.errorresponse)|
|500|[Internal Server Error](https://tools.ietf.org/html/rfc7231#section-6.6.1)|Internal Server Error|[server.errorResponse](#schemaserver.errorresponse)|
|501|[Not Implemented](https://tools.ietf.org/html/rfc7231#section-6.6.2)|Not Implemented|[server.errorResponse](#schemaserver.errorresponse)|

<aside class="warning">
To perform this operation, you must be authenticated by means of one of the following methods:
BearerAuth
</aside>

## List pinned references for a store

> Code samples

```shell
# You can also use wget
curl -X GET /v1/store/{name}/pin \
  -H 'Accept: application/json' \
  -H 'Authorization: API_KEY'

```

`GET /v1/store/{name}/pin`

Pins are exempt from retention GC: exact references, or doublestar patterns matched against the full ref, its name:tag short form, and the bare tag.

<h3 id="list-pinned-references-for-a-store-parameters">Parameters</h3>

|Name|In|Type|Required|Description|
|---|---|---|---|---|
|name|path|string|true|engine store name|

> Example responses

> 200 Response

```json
{
  "pins": [
    {
      "pattern": true,
      "pinned_at": "string",
      "value": "string"
    }
  ]
}
```

<h3 id="list-pinned-references-for-a-store-responses">Responses</h3>

|Status|Meaning|Description|Schema|
|---|---|---|---|
|200|[OK](https://tools.ietf.org/html/rfc7231#section-6.3.1)|OK|[server.pinListResponse](#schemaserver.pinlistresponse)|
|404|[Not Found](https://tools.ietf.org/html/rfc7231#section-6.5.4)|Not Found|[server.errorResponse](#schemaserver.errorresponse)|
|500|[Internal Server Error](https://tools.ietf.org/html/rfc7231#section-6.6.1)|Internal Server Error|[server.errorResponse](#schemaserver.errorresponse)|
|501|[Not Implemented](https://tools.ietf.org/html/rfc7231#section-6.6.2)|Not Implemented|[server.errorResponse](#schemaserver.errorresponse)|

<aside class="warning">
To perform this operation, you must be authenticated by means of one of the following methods:
BearerAuth
</aside>

## Pin a reference or pattern (exempt from GC)

> Code samples

```shell
# You can also use wget
curl -X POST /v1/store/{name}/pin \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json' \
  -H 'Authorization: API_KEY'

```

`POST /v1/store/{name}/pin`

Body carries exactly one of `ref` (exact) or `pattern` (doublestar; matched against the full ref, name:tag, and the bare tag). Preview the effect with the GC dry-run — a broad pattern like `**` disables age GC entirely.

> Body parameter

```json
{
  "pattern": "*:stable",
  "ref": "docker.io/library/nginx:1.27"
}
```

<h3 id="pin-a-reference-or-pattern-(exempt-from-gc)-parameters">Parameters</h3>

|Name|In|Type|Required|Description|
|---|---|---|---|---|
|name|path|string|true|engine store name|
|body|body|[server.pinRequest](#schemaserver.pinrequest)|true|reference or pattern to pin|

> Example responses

> 400 Response

```json
{
  "error": "string"
}
```

<h3 id="pin-a-reference-or-pattern-(exempt-from-gc)-responses">Responses</h3>

|Status|Meaning|Description|Schema|
|---|---|---|---|
|204|[No Content](https://tools.ietf.org/html/rfc7231#section-6.3.5)|No Content|None|
|400|[Bad Request](https://tools.ietf.org/html/rfc7231#section-6.5.1)|Bad Request|[server.errorResponse](#schemaserver.errorresponse)|
|404|[Not Found](https://tools.ietf.org/html/rfc7231#section-6.5.4)|Not Found|[server.errorResponse](#schemaserver.errorresponse)|
|500|[Internal Server Error](https://tools.ietf.org/html/rfc7231#section-6.6.1)|Internal Server Error|[server.errorResponse](#schemaserver.errorresponse)|
|501|[Not Implemented](https://tools.ietf.org/html/rfc7231#section-6.6.2)|Not Implemented|[server.errorResponse](#schemaserver.errorresponse)|

<aside class="warning">
To perform this operation, you must be authenticated by means of one of the following methods:
BearerAuth
</aside>

## Remove an image from an engine store

> Code samples

```shell
# You can also use wget
curl -X POST /v1/store/{name}/remove \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json' \
  -H 'Authorization: API_KEY'

```

`POST /v1/store/{name}/remove`

Manually deletes one image from an engine store and syncs the retention index.

> Body parameter

```json
{
  "ref": "docker.io/library/nginx:1.27"
}
```

<h3 id="remove-an-image-from-an-engine-store-parameters">Parameters</h3>

|Name|In|Type|Required|Description|
|---|---|---|---|---|
|name|path|string|true|engine store name|
|body|body|[server.removeRequest](#schemaserver.removerequest)|true|reference to remove|

> Example responses

> 200 Response

```json
{
  "deleted": [
    "string"
  ],
  "untagged": [
    "string"
  ]
}
```

<h3 id="remove-an-image-from-an-engine-store-responses">Responses</h3>

|Status|Meaning|Description|Schema|
|---|---|---|---|
|200|[OK](https://tools.ietf.org/html/rfc7231#section-6.3.1)|OK|[down.RemoveResult](#schemadown.removeresult)|
|400|[Bad Request](https://tools.ietf.org/html/rfc7231#section-6.5.1)|Bad Request|[server.errorResponse](#schemaserver.errorresponse)|
|404|[Not Found](https://tools.ietf.org/html/rfc7231#section-6.5.4)|Not Found|[server.errorResponse](#schemaserver.errorresponse)|
|502|[Bad Gateway](https://tools.ietf.org/html/rfc7231#section-6.6.3)|Bad Gateway|[server.errorResponse](#schemaserver.errorresponse)|

<aside class="warning">
To perform this operation, you must be authenticated by means of one of the following methods:
BearerAuth
</aside>

## Usage-watcher status for a store

> Code samples

```shell
# You can also use wget
curl -X GET /v1/store/{name}/watcher \
  -H 'Accept: application/json' \
  -H 'Authorization: API_KEY'

```

`GET /v1/store/{name}/watcher`

Liveness of the engine's usage-event stream. A dead stream freezes last-used times and silently degrades age GC — alert on connected=false or a stale last_event_at.

<h3 id="usage-watcher-status-for-a-store-parameters">Parameters</h3>

|Name|In|Type|Required|Description|
|---|---|---|---|---|
|name|path|string|true|engine store name|

> Example responses

> 200 Response

```json
{
  "connected": true,
  "last_error": "string",
  "last_event_at": "string",
  "last_seed_at": "string",
  "reconnects": 0,
  "watching_since": "string"
}
```

<h3 id="usage-watcher-status-for-a-store-responses">Responses</h3>

|Status|Meaning|Description|Schema|
|---|---|---|---|
|200|[OK](https://tools.ietf.org/html/rfc7231#section-6.3.1)|OK|[retention.WatcherStatus](#schemaretention.watcherstatus)|
|404|[Not Found](https://tools.ietf.org/html/rfc7231#section-6.5.4)|Not Found|[server.errorResponse](#schemaserver.errorresponse)|
|501|[Not Implemented](https://tools.ietf.org/html/rfc7231#section-6.6.2)|Not Implemented|[server.errorResponse](#schemaserver.errorresponse)|

<aside class="warning">
To perform this operation, you must be authenticated by means of one of the following methods:
BearerAuth
</aside>

<h1 id="gantry-api-jobs">jobs</h1>

## List jobs

> Code samples

```shell
# You can also use wget
curl -X GET /v1/job \
  -H 'Accept: application/json' \
  -H 'Authorization: API_KEY'

```

`GET /v1/job`

<h3 id="list-jobs-parameters">Parameters</h3>

|Name|In|Type|Required|Description|
|---|---|---|---|---|
|state|query|string|false|filter by state|
|ref|query|string|false|filter by ref substring|

> Example responses

> 200 Response

```json
{
  "items": [
    {
      "created_at": "string",
      "ended_at": "string",
      "error": "string",
      "id": "string",
      "platforms": [
        "string"
      ],
      "ref": "string",
      "started_at": "string",
      "state": "pending",
      "transfers": [
        {
          "bytes_done": 0,
          "bytes_total": 0,
          "digest": "string",
          "error": "string",
          "from": "string",
          "kind": "oci",
          "layers": [
            {
              "digest": "string",
              "done": 0,
              "platform": "string",
              "state": "pending",
              "total": 0
            }
          ],
          "ref": "string",
          "state": "pending",
          "store": "string"
        }
      ],
      "verification": {
        "digest": "string",
        "mode": "off",
        "verified": true
      }
    }
  ]
}
```

<h3 id="list-jobs-responses">Responses</h3>

|Status|Meaning|Description|Schema|
|---|---|---|---|
|200|[OK](https://tools.ietf.org/html/rfc7231#section-6.3.1)|OK|[server.jobListResponse](#schemaserver.joblistresponse)|

<aside class="warning">
To perform this operation, you must be authenticated by means of one of the following methods:
BearerAuth
</aside>

## Create a job

> Code samples

```shell
# You can also use wget
curl -X POST /v1/job \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json' \
  -H 'Authorization: API_KEY'

```

`POST /v1/job`

Move an image: copy `from` (oci) into `to` (oci), then have the `distribute` engines pull it, anchored to the digest the copy committed. Idempotent per identical move.

> Body parameter

```json
{
  "copy_referrers": true,
  "distribute": [
    "node-a",
    "node-b"
  ],
  "from": "dockerhub",
  "platforms": [
    "linux/amd64",
    "linux/arm64"
  ],
  "ref": "docker.io/library/nginx:1.27",
  "to": "local-cache"
}
```

<h3 id="create-a-job-parameters">Parameters</h3>

|Name|In|Type|Required|Description|
|---|---|---|---|---|
|body|body|[server.createJobRequest](#schemaserver.createjobrequest)|true|job request|

> Example responses

> 202 Response

```json
{
  "created_at": "string",
  "ended_at": "string",
  "error": "string",
  "id": "string",
  "platforms": [
    "string"
  ],
  "ref": "string",
  "started_at": "string",
  "state": "pending",
  "transfers": [
    {
      "bytes_done": 0,
      "bytes_total": 0,
      "digest": "string",
      "error": "string",
      "from": "string",
      "kind": "oci",
      "layers": [
        {
          "digest": "string",
          "done": 0,
          "platform": "string",
          "state": "pending",
          "total": 0
        }
      ],
      "ref": "string",
      "state": "pending",
      "store": "string"
    }
  ],
  "verification": {
    "digest": "string",
    "mode": "off",
    "verified": true
  }
}
```

<h3 id="create-a-job-responses">Responses</h3>

|Status|Meaning|Description|Schema|
|---|---|---|---|
|202|[Accepted](https://tools.ietf.org/html/rfc7231#section-6.3.3)|Accepted|[warm.JobSnapshot](#schemawarm.jobsnapshot)|
|400|[Bad Request](https://tools.ietf.org/html/rfc7231#section-6.5.1)|Bad Request|[server.errorResponse](#schemaserver.errorresponse)|
|422|[Unprocessable Entity](https://tools.ietf.org/html/rfc2518#section-10.3)|source image signature verification failed|[server.errorResponse](#schemaserver.errorresponse)|
|503|[Service Unavailable](https://tools.ietf.org/html/rfc7231#section-6.6.4)|Service Unavailable|[server.errorResponse](#schemaserver.errorresponse)|

### Response Headers

|Status|Header|Type|Format|Description|
|---|---|---|---|---|
|202|Location|string||canonical URL of the created job (/v1/job/{id})|

<aside class="warning">
To perform this operation, you must be authenticated by means of one of the following methods:
BearerAuth
</aside>

## Cancel or evict a job

> Code samples

```shell
# You can also use wget
curl -X DELETE /v1/job/{id} \
  -H 'Accept: application/json' \
  -H 'Authorization: API_KEY'

```

`DELETE /v1/job/{id}`

<h3 id="cancel-or-evict-a-job-parameters">Parameters</h3>

|Name|In|Type|Required|Description|
|---|---|---|---|---|
|id|path|string|true|job id|

> Example responses

> 404 Response

```json
{
  "error": "string"
}
```

<h3 id="cancel-or-evict-a-job-responses">Responses</h3>

|Status|Meaning|Description|Schema|
|---|---|---|---|
|204|[No Content](https://tools.ietf.org/html/rfc7231#section-6.3.5)|No Content|None|
|404|[Not Found](https://tools.ietf.org/html/rfc7231#section-6.5.4)|Not Found|[server.errorResponse](#schemaserver.errorresponse)|

<aside class="warning">
To perform this operation, you must be authenticated by means of one of the following methods:
BearerAuth
</aside>

## Get a job

> Code samples

```shell
# You can also use wget
curl -X GET /v1/job/{id} \
  -H 'Accept: application/json' \
  -H 'Authorization: API_KEY'

```

`GET /v1/job/{id}`

<h3 id="get-a-job-parameters">Parameters</h3>

|Name|In|Type|Required|Description|
|---|---|---|---|---|
|id|path|string|true|job id|

> Example responses

> 200 Response

```json
{
  "created_at": "string",
  "ended_at": "string",
  "error": "string",
  "id": "string",
  "platforms": [
    "string"
  ],
  "ref": "string",
  "started_at": "string",
  "state": "pending",
  "transfers": [
    {
      "bytes_done": 0,
      "bytes_total": 0,
      "digest": "string",
      "error": "string",
      "from": "string",
      "kind": "oci",
      "layers": [
        {
          "digest": "string",
          "done": 0,
          "platform": "string",
          "state": "pending",
          "total": 0
        }
      ],
      "ref": "string",
      "state": "pending",
      "store": "string"
    }
  ],
  "verification": {
    "digest": "string",
    "mode": "off",
    "verified": true
  }
}
```

<h3 id="get-a-job-responses">Responses</h3>

|Status|Meaning|Description|Schema|
|---|---|---|---|
|200|[OK](https://tools.ietf.org/html/rfc7231#section-6.3.1)|OK|[warm.JobSnapshot](#schemawarm.jobsnapshot)|
|404|[Not Found](https://tools.ietf.org/html/rfc7231#section-6.5.4)|Not Found|[server.errorResponse](#schemaserver.errorresponse)|

<aside class="warning">
To perform this operation, you must be authenticated by means of one of the following methods:
BearerAuth
</aside>

## Stream job progress

> Code samples

```shell
# You can also use wget
curl -X GET /v1/job/{id}/progress \
  -H 'Accept: text/event-stream' \
  -H 'Authorization: API_KEY'

```

`GET /v1/job/{id}/progress`

Streams Server-Sent Events: repeated `event: progress` frames each carrying a JSON warm.JobSnapshot in `data:`, ending with a terminal `event: done` frame. With ?wait=<dur> it instead long-polls and returns a single JSON warm.JobSnapshot (no SSE framing) once the job is terminal or the wait elapses.

<h3 id="stream-job-progress-parameters">Parameters</h3>

|Name|In|Type|Required|Description|
|---|---|---|---|---|
|id|path|string|true|job id|
|wait|query|string|false|long-poll duration, e.g. 30s|

> Example responses

> 200 Response

<h3 id="stream-job-progress-responses">Responses</h3>

|Status|Meaning|Description|Schema|
|---|---|---|---|
|200|[OK](https://tools.ietf.org/html/rfc7231#section-6.3.1)|OK|[warm.JobSnapshot](#schemawarm.jobsnapshot)|
|404|[Not Found](https://tools.ietf.org/html/rfc7231#section-6.5.4)|Not Found|[server.errorResponse](#schemaserver.errorresponse)|
|500|[Internal Server Error](https://tools.ietf.org/html/rfc7231#section-6.6.1)|Internal Server Error|[server.errorResponse](#schemaserver.errorresponse)|

<aside class="warning">
To perform this operation, you must be authenticated by means of one of the following methods:
BearerAuth
</aside>

<h1 id="gantry-api-stores">stores</h1>

## List stores

> Code samples

```shell
# You can also use wget
curl -X GET /v1/store \
  -H 'Accept: application/json' \
  -H 'Authorization: API_KEY'

```

`GET /v1/store`

Configured stores with their kind, capabilities, and readiness.

> Example responses

> 200 Response

```json
{
  "items": [
    {
      "address": "string",
      "capabilities": {
        "gc": true,
        "pull": true,
        "read": true,
        "verify": true,
        "write": true
      },
      "error": "string",
      "host": "string",
      "kind": "oci",
      "mode": "string",
      "name": "string",
      "namespace": "string",
      "ready": true
    }
  ]
}
```

<h3 id="list-stores-responses">Responses</h3>

|Status|Meaning|Description|Schema|
|---|---|---|---|
|200|[OK](https://tools.ietf.org/html/rfc7231#section-6.3.1)|OK|[server.storeListResponse](#schemaserver.storelistresponse)|

<aside class="warning">
To perform this operation, you must be authenticated by means of one of the following methods:
BearerAuth
</aside>

## Check a store's health

> Code samples

```shell
# You can also use wget
curl -X GET /v1/store/{name}/health \
  -H 'Accept: application/json' \
  -H 'Authorization: API_KEY'

```

`GET /v1/store/{name}/health`

Probes one store's reachability (an engine daemon ready-check, or a registry GET /v2/ ping) and returns the result. The probe is cached for a short TTL (serve.health.cache_ttl, default 5s); a cached response sets `cached: true`. Returns 200 when healthy, 503 when unhealthy (report body either way), 404 for an unknown store.

<h3 id="check-a-store's-health-parameters">Parameters</h3>

|Name|In|Type|Required|Description|
|---|---|---|---|---|
|name|path|string|true|store name|

> Example responses

> 200 Response

```json
{
  "cached": true,
  "checked_at": "string",
  "error": "string",
  "healthy": true,
  "kind": "string",
  "latency_ms": 0,
  "name": "string"
}
```

<h3 id="check-a-store's-health-responses">Responses</h3>

|Status|Meaning|Description|Schema|
|---|---|---|---|
|200|[OK](https://tools.ietf.org/html/rfc7231#section-6.3.1)|OK|[health.Report](#schemahealth.report)|
|404|[Not Found](https://tools.ietf.org/html/rfc7231#section-6.5.4)|Not Found|[server.errorResponse](#schemaserver.errorresponse)|
|503|[Service Unavailable](https://tools.ietf.org/html/rfc7231#section-6.6.4)|Service Unavailable|[health.Report](#schemahealth.report)|

<aside class="warning">
To perform this operation, you must be authenticated by means of one of the following methods:
BearerAuth
</aside>

## List images live containers hold on an engine

> Code samples

```shell
# You can also use wget
curl -X GET /v1/store/{name}/inuse \
  -H 'Accept: application/json' \
  -H 'Authorization: API_KEY'

```

`GET /v1/store/{name}/inuse`

The references and image IDs running containers use right now — GC's top protection tier, as ground truth (probed live, not from the index).

<h3 id="list-images-live-containers-hold-on-an-engine-parameters">Parameters</h3>

|Name|In|Type|Required|Description|
|---|---|---|---|---|
|name|path|string|true|engine store name|

> Example responses

> 200 Response

```json
{
  "in_use": [
    "string"
  ]
}
```

<h3 id="list-images-live-containers-hold-on-an-engine-responses">Responses</h3>

|Status|Meaning|Description|Schema|
|---|---|---|---|
|200|[OK](https://tools.ietf.org/html/rfc7231#section-6.3.1)|OK|[server.inUseResponse](#schemaserver.inuseresponse)|
|404|[Not Found](https://tools.ietf.org/html/rfc7231#section-6.5.4)|Not Found|[server.errorResponse](#schemaserver.errorresponse)|
|502|[Bad Gateway](https://tools.ietf.org/html/rfc7231#section-6.6.3)|Bad Gateway|[server.errorResponse](#schemaserver.errorresponse)|

<aside class="warning">
To perform this operation, you must be authenticated by means of one of the following methods:
BearerAuth
</aside>

## Trigger an engine store to pull

> Code samples

```shell
# You can also use wget
curl -X POST /v1/store/{name}/pull \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json' \
  -H 'Authorization: API_KEY'

```

`POST /v1/store/{name}/pull`

Tells one engine store to pull a reference, decoupled from the job pipeline (manual reconcile). Stamps the retention index like a job's distribute step, so the pulled image stays eligible for age GC.

> Body parameter

```json
{
  "ref": "docker.io/library/nginx:1.27"
}
```

<h3 id="trigger-an-engine-store-to-pull-parameters">Parameters</h3>

|Name|In|Type|Required|Description|
|---|---|---|---|---|
|name|path|string|true|engine store name|
|body|body|[server.storePullRequest](#schemaserver.storepullrequest)|true|reference to pull|

> Example responses

> 200 Response

```json
{
  "kind": "string",
  "ref": "string",
  "state": "string",
  "store": "string"
}
```

<h3 id="trigger-an-engine-store-to-pull-responses">Responses</h3>

|Status|Meaning|Description|Schema|
|---|---|---|---|
|200|[OK](https://tools.ietf.org/html/rfc7231#section-6.3.1)|OK|[server.storePullResponse](#schemaserver.storepullresponse)|
|400|[Bad Request](https://tools.ietf.org/html/rfc7231#section-6.5.1)|Bad Request|[server.errorResponse](#schemaserver.errorresponse)|
|404|[Not Found](https://tools.ietf.org/html/rfc7231#section-6.5.4)|Not Found|[server.errorResponse](#schemaserver.errorresponse)|
|502|[Bad Gateway](https://tools.ietf.org/html/rfc7231#section-6.6.3)|Bad Gateway|[server.errorResponse](#schemaserver.errorresponse)|

<aside class="warning">
To perform this operation, you must be authenticated by means of one of the following methods:
BearerAuth
</aside>

<h1 id="gantry-api-verify">verify</h1>

## Effective verification configuration

> Code samples

```shell
# You can also use wget
curl -X GET /v1/verify \
  -H 'Accept: application/json' \
  -H 'Authorization: API_KEY'

```

`GET /v1/verify`

The trust configuration in effect: provider, global and per-store modes, trust-policy scopes and identities, and each trust anchor's subject, fingerprint, and expiry (never key material). Anchor expiry is an outage in require mode — alert on not_after.

> Example responses

> 200 Response

```json
{
  "anchors": [
    {
      "fingerprint": "string",
      "not_after": "string",
      "subject": "string"
    }
  ],
  "enabled": true,
  "level": "string",
  "mode": "string",
  "policies": [
    {
      "name": "string",
      "registry_scopes": [
        "string"
      ],
      "trusted_identities": [
        "string"
      ],
      "verification_level": "string"
    }
  ],
  "provider": "string",
  "stores": {
    "property1": "string",
    "property2": "string"
  }
}
```

<h3 id="effective-verification-configuration-responses">Responses</h3>

|Status|Meaning|Description|Schema|
|---|---|---|---|
|200|[OK](https://tools.ietf.org/html/rfc7231#section-6.3.1)|OK|[server.verifyDescribeResponse](#schemaserver.verifydescriberesponse)|

<aside class="warning">
To perform this operation, you must be authenticated by means of one of the following methods:
BearerAuth
</aside>

## Verify a reference without moving it

> Code samples

```shell
# You can also use wget
curl -X POST /v1/verify \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json' \
  -H 'Authorization: API_KEY'

```

`POST /v1/verify`

Runs the same notation verification job admission uses — resolve the tag on its source store, check the signature against the trust policy — without moving bytes or creating a job. Lets CI gate a rollout on "this gantry will accept the image".

> Body parameter

```json
{
  "from": "dockerhub",
  "ref": "docker.io/library/nginx:1.27"
}
```

<h3 id="verify-a-reference-without-moving-it-parameters">Parameters</h3>

|Name|In|Type|Required|Description|
|---|---|---|---|---|
|body|body|[server.verifyRequest](#schemaserver.verifyrequest)|true|reference to verify|

> Example responses

> 200 Response

```json
{
  "digest": "string",
  "mode": "off",
  "ref": "string",
  "verified": true
}
```

<h3 id="verify-a-reference-without-moving-it-responses">Responses</h3>

|Status|Meaning|Description|Schema|
|---|---|---|---|
|200|[OK](https://tools.ietf.org/html/rfc7231#section-6.3.1)|OK|[server.verifyResponse](#schemaserver.verifyresponse)|
|400|[Bad Request](https://tools.ietf.org/html/rfc7231#section-6.5.1)|Bad Request|[server.errorResponse](#schemaserver.errorresponse)|
|404|[Not Found](https://tools.ietf.org/html/rfc7231#section-6.5.4)|reference not found on the source|[server.errorResponse](#schemaserver.errorresponse)|
|422|[Unprocessable Entity](https://tools.ietf.org/html/rfc2518#section-10.3)|unsigned, or signature verification failed|[server.errorResponse](#schemaserver.errorresponse)|
|501|[Not Implemented](https://tools.ietf.org/html/rfc7231#section-6.6.2)|Not Implemented|[server.errorResponse](#schemaserver.errorresponse)|
|502|[Bad Gateway](https://tools.ietf.org/html/rfc7231#section-6.6.3)|verification could not be completed|[server.errorResponse](#schemaserver.errorresponse)|

<aside class="warning">
To perform this operation, you must be authenticated by means of one of the following methods:
BearerAuth
</aside>

## Reload the trust store and policy

> Code samples

```shell
# You can also use wget
curl -X POST /v1/verify/reload \
  -H 'Accept: application/json' \
  -H 'Authorization: API_KEY'

```

`POST /v1/verify/reload`

Re-reads the CA directory and trust policy from disk with the same fail-fast validation as startup, atomically swapping the verifier on success. On failure the previous verifier stays active. Makes CA/leaf rotation a fleet-safe operation without restarting gantry.

> Example responses

> 200 Response

```json
{
  "anchors": [
    {
      "fingerprint": "string",
      "not_after": "string",
      "subject": "string"
    }
  ],
  "enabled": true,
  "level": "string",
  "mode": "string",
  "policies": [
    {
      "name": "string",
      "registry_scopes": [
        "string"
      ],
      "trusted_identities": [
        "string"
      ],
      "verification_level": "string"
    }
  ],
  "provider": "string"
}
```

<h3 id="reload-the-trust-store-and-policy-responses">Responses</h3>

|Status|Meaning|Description|Schema|
|---|---|---|---|
|200|[OK](https://tools.ietf.org/html/rfc7231#section-6.3.1)|the newly loaded configuration|[verify.Description](#schemaverify.description)|
|422|[Unprocessable Entity](https://tools.ietf.org/html/rfc2518#section-10.3)|new trust material rejected; old verifier retained|[server.errorResponse](#schemaserver.errorresponse)|
|500|[Internal Server Error](https://tools.ietf.org/html/rfc7231#section-6.6.1)|reload could not complete (transient); old verifier retained|[server.errorResponse](#schemaserver.errorresponse)|
|501|[Not Implemented](https://tools.ietf.org/html/rfc7231#section-6.6.2)|Not Implemented|[server.errorResponse](#schemaserver.errorresponse)|

<aside class="warning">
To perform this operation, you must be authenticated by means of one of the following methods:
BearerAuth
</aside>

# Schemas

<h2 id="tocS_down.RemoveResult">down.RemoveResult</h2>
<!-- backwards compatibility -->
<a id="schemadown.removeresult"></a>
<a id="schema_down.RemoveResult"></a>
<a id="tocSdown.removeresult"></a>
<a id="tocsdown.removeresult"></a>

```json
{
  "deleted": [
    "string"
  ],
  "untagged": [
    "string"
  ]
}

```

### Properties

|Name|Type|Required|Restrictions|Description|
|---|---|---|---|---|
|deleted|[string]|false|none|content IDs whose bytes were actually deleted|
|untagged|[string]|false|none|tag refs removed; disk freed only when the last tag/content GCs|

<h2 id="tocS_health.Report">health.Report</h2>
<!-- backwards compatibility -->
<a id="schemahealth.report"></a>
<a id="schema_health.Report"></a>
<a id="tocShealth.report"></a>
<a id="tocshealth.report"></a>

```json
{
  "cached": true,
  "checked_at": "string",
  "error": "string",
  "healthy": true,
  "kind": "string",
  "latency_ms": 0,
  "name": "string"
}

```

### Properties

|Name|Type|Required|Restrictions|Description|
|---|---|---|---|---|
|cached|boolean|false|none|Cached is true when this report was served from the TTL cache rather than<br>probed for this request.|
|checked_at|string|false|none|none|
|error|string|false|none|none|
|healthy|boolean|false|none|none|
|kind|string|false|none|none|
|latency_ms|integer|false|none|none|
|name|string|false|none|none|

<h2 id="tocS_retention.ApplyResult">retention.ApplyResult</h2>
<!-- backwards compatibility -->
<a id="schemaretention.applyresult"></a>
<a id="schema_retention.ApplyResult"></a>
<a id="tocSretention.applyresult"></a>
<a id="tocsretention.applyresult"></a>

```json
{
  "deleted": [
    "string"
  ],
  "errors": [
    "string"
  ],
  "evaluated": 0,
  "untagged": [
    "string"
  ]
}

```

### Properties

|Name|Type|Required|Restrictions|Description|
|---|---|---|---|---|
|deleted|[string]|false|none|content-hash IDs whose bytes were freed|
|errors|[string]|false|none|per-ref removal failures, "<ref>: <err>"|
|evaluated|integer|false|none|number of records considered (delete+keep)|
|untagged|[string]|false|none|refs whose tag was removed but content may remain|

<h2 id="tocS_retention.Candidate">retention.Candidate</h2>
<!-- backwards compatibility -->
<a id="schemaretention.candidate"></a>
<a id="schema_retention.Candidate"></a>
<a id="tocSretention.candidate"></a>
<a id="tocsretention.candidate"></a>

```json
{
  "digest": "string",
  "last_used": "string",
  "reason": "string",
  "ref": "string"
}

```

### Properties

|Name|Type|Required|Restrictions|Description|
|---|---|---|---|---|
|digest|string|false|none|none|
|last_used|string|false|none|none|
|reason|string|false|none|age_exceeded|
|ref|string|false|none|none|

<h2 id="tocS_retention.Decision">retention.Decision</h2>
<!-- backwards compatibility -->
<a id="schemaretention.decision"></a>
<a id="schema_retention.Decision"></a>
<a id="tocSretention.decision"></a>
<a id="tocsretention.decision"></a>

```json
{
  "delete": [
    {
      "digest": "string",
      "last_used": "string",
      "reason": "string",
      "ref": "string"
    }
  ],
  "keep": [
    {
      "reason": "string",
      "ref": "string"
    }
  ],
  "next_age_out": "string"
}

```

### Properties

|Name|Type|Required|Restrictions|Description|
|---|---|---|---|---|
|delete|[[retention.Candidate](#schemaretention.candidate)]|false|none|none|
|keep|[[retention.Kept](#schemaretention.kept)]|false|none|none|
|next_age_out|string|false|none|NextAgeOut is the soonest a currently-kept record could become<br>age-deletable; the scheduler waits until then (or a usage event). Zero<br>means nothing is on an age path.|

<h2 id="tocS_retention.Kept">retention.Kept</h2>
<!-- backwards compatibility -->
<a id="schemaretention.kept"></a>
<a id="schema_retention.Kept"></a>
<a id="tocSretention.kept"></a>
<a id="tocsretention.kept"></a>

```json
{
  "reason": "string",
  "ref": "string"
}

```

### Properties

|Name|Type|Required|Restrictions|Description|
|---|---|---|---|---|
|reason|string|false|none|in_use | pinned | keep_n_recent | within_max_age | grace | age_gc_disabled|
|ref|string|false|none|none|

<h2 id="tocS_retention.PinEntry">retention.PinEntry</h2>
<!-- backwards compatibility -->
<a id="schemaretention.pinentry"></a>
<a id="schema_retention.PinEntry"></a>
<a id="tocSretention.pinentry"></a>
<a id="tocsretention.pinentry"></a>

```json
{
  "pattern": true,
  "pinned_at": "string",
  "value": "string"
}

```

### Properties

|Name|Type|Required|Restrictions|Description|
|---|---|---|---|---|
|pattern|boolean|false|none|none|
|pinned_at|string|false|none|zero for pins stored before entries carried a timestamp|
|value|string|false|none|none|

<h2 id="tocS_retention.PolicyStatus">retention.PolicyStatus</h2>
<!-- backwards compatibility -->
<a id="schemaretention.policystatus"></a>
<a id="schema_retention.PolicyStatus"></a>
<a id="tocSretention.policystatus"></a>
<a id="tocsretention.policystatus"></a>

```json
{
  "keep_n": 0,
  "max_age": "string",
  "pins": [
    "string"
  ]
}

```

### Properties

|Name|Type|Required|Restrictions|Description|
|---|---|---|---|---|
|keep_n|integer|false|none|none|
|max_age|string|false|none|none|
|pins|[string]|false|none|none|

<h2 id="tocS_retention.Record">retention.Record</h2>
<!-- backwards compatibility -->
<a id="schemaretention.record"></a>
<a id="schema_retention.Record"></a>
<a id="tocSretention.record"></a>
<a id="tocsretention.record"></a>

```json
{
  "digest": "string",
  "first_seen": "string",
  "last_distributed": "string",
  "last_used": "string",
  "pinned": true,
  "ref": "string",
  "repo": "string",
  "tag": "string"
}

```

### Properties

|Name|Type|Required|Restrictions|Description|
|---|---|---|---|---|
|digest|string|false|none|resolved manifest digest, if known|
|first_seen|string|false|none|none|
|last_distributed|string|false|none|none|
|last_used|string|false|none|the last-USED signal; zero => never observed used|
|pinned|boolean|false|none|explicit pin on this exact ref|
|ref|string|false|none|full local ref incl. tag, e.g. "cache.local/team/app:1.2"|
|repo|string|false|none|RepositoryStr(); the keep-N grouping key|
|tag|string|false|none|tag portion; "" for digest refs|

<h2 id="tocS_retention.ScheduleStatus">retention.ScheduleStatus</h2>
<!-- backwards compatibility -->
<a id="schemaretention.schedulestatus"></a>
<a id="schema_retention.ScheduleStatus"></a>
<a id="tocSretention.schedulestatus"></a>
<a id="tocsretention.schedulestatus"></a>

```json
{
  "grace": "string",
  "interval": "string",
  "min_interval": "string"
}

```

### Properties

|Name|Type|Required|Restrictions|Description|
|---|---|---|---|---|
|grace|string|false|none|none|
|interval|string|false|none|none|
|min_interval|string|false|none|none|

<h2 id="tocS_retention.Status">retention.Status</h2>
<!-- backwards compatibility -->
<a id="schemaretention.status"></a>
<a id="schema_retention.Status"></a>
<a id="tocSretention.status"></a>
<a id="tocsretention.status"></a>

```json
{
  "enabled": true,
  "grace_until": "string",
  "last_run": "string",
  "next_wake": "string",
  "policy": {
    "keep_n": 0,
    "max_age": "string",
    "pins": [
      "string"
    ]
  },
  "running": true,
  "schedule": {
    "grace": "string",
    "interval": "string",
    "min_interval": "string"
  },
  "started": "string",
  "stores": {
    "property1": {
      "pins": 0,
      "records": 0
    },
    "property2": {
      "pins": 0,
      "records": 0
    }
  }
}

```

### Properties

|Name|Type|Required|Restrictions|Description|
|---|---|---|---|---|
|enabled|boolean|false|none|scheduler running (interval > 0)|
|grace_until|string|false|none|age deletion held off until then|
|last_run|string|false|none|none|
|next_wake|string|false|none|none|
|policy|[retention.PolicyStatus](#schemaretention.policystatus)|false|none|none|
|running|boolean|false|none|a GC pass is executing right now|
|schedule|[retention.ScheduleStatus](#schemaretention.schedulestatus)|false|none|none|
|started|string|false|none|none|
|stores|object|false|none|per-engine index sizes|
|» **additionalProperties**|[retention.StoreCounts](#schemaretention.storecounts)|false|none|none|

<h2 id="tocS_retention.StoreCounts">retention.StoreCounts</h2>
<!-- backwards compatibility -->
<a id="schemaretention.storecounts"></a>
<a id="schema_retention.StoreCounts"></a>
<a id="tocSretention.storecounts"></a>
<a id="tocsretention.storecounts"></a>

```json
{
  "pins": 0,
  "records": 0
}

```

### Properties

|Name|Type|Required|Restrictions|Description|
|---|---|---|---|---|
|pins|integer|false|none|none|
|records|integer|false|none|none|

<h2 id="tocS_retention.WatcherStatus">retention.WatcherStatus</h2>
<!-- backwards compatibility -->
<a id="schemaretention.watcherstatus"></a>
<a id="schema_retention.WatcherStatus"></a>
<a id="tocSretention.watcherstatus"></a>
<a id="tocsretention.watcherstatus"></a>

```json
{
  "connected": true,
  "last_error": "string",
  "last_event_at": "string",
  "last_seed_at": "string",
  "reconnects": 0,
  "watching_since": "string"
}

```

### Properties

|Name|Type|Required|Restrictions|Description|
|---|---|---|---|---|
|connected|boolean|false|none|none|
|last_error|string|false|none|none|
|last_event_at|string|false|none|receipt time of the last usage event|
|last_seed_at|string|false|none|none|
|reconnects|integer|false|none|times the event stream ended and was re-established|
|watching_since|string|false|none|none|

<h2 id="tocS_server.createJobRequest">server.createJobRequest</h2>
<!-- backwards compatibility -->
<a id="schemaserver.createjobrequest"></a>
<a id="schema_server.createJobRequest"></a>
<a id="tocSserver.createjobrequest"></a>
<a id="tocsserver.createjobrequest"></a>

```json
{
  "copy_referrers": true,
  "distribute": [
    "node-a",
    "node-b"
  ],
  "from": "dockerhub",
  "platforms": [
    "linux/amd64",
    "linux/arm64"
  ],
  "ref": "docker.io/library/nginx:1.27",
  "to": "local-cache"
}

```

### Properties

|Name|Type|Required|Restrictions|Description|
|---|---|---|---|---|
|copy_referrers|boolean|false|none|Copy the source's referrer artifacts (notation signatures) into `to` with<br>the source digest preserved, so the image still verifies against the cache.<br>Requires copying all platforms (the request must not narrow `platforms`).<br>Defaults to true when verification is enabled and `to` is a copy-mode store.|
|distribute|[string]|false|none|Engine store names that should pull the image afterwards.|
|from|string|false|none|Source registry store name or host; defaults to the ref's registry.|
|platforms|[string]|false|none|Platforms to move; defaults to the server platform when empty.|
|ref|string|true|none|Image reference to move (required).|
|to|string|false|none|Destination registry store to copy into; empty means engines pull from `from` directly.|

<h2 id="tocS_server.errorResponse">server.errorResponse</h2>
<!-- backwards compatibility -->
<a id="schemaserver.errorresponse"></a>
<a id="schema_server.errorResponse"></a>
<a id="tocSserver.errorresponse"></a>
<a id="tocsserver.errorresponse"></a>

```json
{
  "error": "string"
}

```

### Properties

|Name|Type|Required|Restrictions|Description|
|---|---|---|---|---|
|error|string|false|none|none|

<h2 id="tocS_server.gcRequest">server.gcRequest</h2>
<!-- backwards compatibility -->
<a id="schemaserver.gcrequest"></a>
<a id="schema_server.gcRequest"></a>
<a id="tocSserver.gcrequest"></a>
<a id="tocsserver.gcrequest"></a>

```json
{
  "keep_n": 3,
  "max_age": "720h",
  "pins": [
    "docker.io/library/nginx:1.27"
  ]
}

```

### Properties

|Name|Type|Required|Restrictions|Description|
|---|---|---|---|---|
|keep_n|integer|false|none|Override how many most-recent tags to keep per repo; 0 disables keep-N.|
|max_age|string|false|none|Override max image age (Go duration, e.g. "720h"); "0s" disables age GC.|
|pins|[string]|false|none|Override the pinned references exempt from GC.|

<h2 id="tocS_server.imageListResponse">server.imageListResponse</h2>
<!-- backwards compatibility -->
<a id="schemaserver.imagelistresponse"></a>
<a id="schema_server.imageListResponse"></a>
<a id="tocSserver.imagelistresponse"></a>
<a id="tocsserver.imagelistresponse"></a>

```json
{
  "items": [
    {
      "digest": "string",
      "first_seen": "string",
      "last_distributed": "string",
      "last_used": "string",
      "pinned": true,
      "ref": "string",
      "repo": "string",
      "tag": "string"
    }
  ]
}

```

### Properties

|Name|Type|Required|Restrictions|Description|
|---|---|---|---|---|
|items|[[retention.Record](#schemaretention.record)]|false|none|none|

<h2 id="tocS_server.inUseResponse">server.inUseResponse</h2>
<!-- backwards compatibility -->
<a id="schemaserver.inuseresponse"></a>
<a id="schema_server.inUseResponse"></a>
<a id="tocSserver.inuseresponse"></a>
<a id="tocsserver.inuseresponse"></a>

```json
{
  "in_use": [
    "string"
  ]
}

```

### Properties

|Name|Type|Required|Restrictions|Description|
|---|---|---|---|---|
|in_use|[string]|false|none|InUse mixes tag references and image IDs (sha256:...), exactly as live<br>containers reference them.|

<h2 id="tocS_server.jobListResponse">server.jobListResponse</h2>
<!-- backwards compatibility -->
<a id="schemaserver.joblistresponse"></a>
<a id="schema_server.jobListResponse"></a>
<a id="tocSserver.joblistresponse"></a>
<a id="tocsserver.joblistresponse"></a>

```json
{
  "items": [
    {
      "created_at": "string",
      "ended_at": "string",
      "error": "string",
      "id": "string",
      "platforms": [
        "string"
      ],
      "ref": "string",
      "started_at": "string",
      "state": "pending",
      "transfers": [
        {
          "bytes_done": 0,
          "bytes_total": 0,
          "digest": "string",
          "error": "string",
          "from": "string",
          "kind": "oci",
          "layers": [
            {
              "digest": "string",
              "done": 0,
              "platform": "string",
              "state": "pending",
              "total": 0
            }
          ],
          "ref": "string",
          "state": "pending",
          "store": "string"
        }
      ],
      "verification": {
        "digest": "string",
        "mode": "off",
        "verified": true
      }
    }
  ]
}

```

### Properties

|Name|Type|Required|Restrictions|Description|
|---|---|---|---|---|
|items|[[warm.JobSnapshot](#schemawarm.jobsnapshot)]|false|none|none|

<h2 id="tocS_server.pinListResponse">server.pinListResponse</h2>
<!-- backwards compatibility -->
<a id="schemaserver.pinlistresponse"></a>
<a id="schema_server.pinListResponse"></a>
<a id="tocSserver.pinlistresponse"></a>
<a id="tocsserver.pinlistresponse"></a>

```json
{
  "pins": [
    {
      "pattern": true,
      "pinned_at": "string",
      "value": "string"
    }
  ]
}

```

### Properties

|Name|Type|Required|Restrictions|Description|
|---|---|---|---|---|
|pins|[[retention.PinEntry](#schemaretention.pinentry)]|false|none|none|

<h2 id="tocS_server.pinRequest">server.pinRequest</h2>
<!-- backwards compatibility -->
<a id="schemaserver.pinrequest"></a>
<a id="schema_server.pinRequest"></a>
<a id="tocSserver.pinrequest"></a>
<a id="tocsserver.pinrequest"></a>

```json
{
  "pattern": "*:stable",
  "ref": "docker.io/library/nginx:1.27"
}

```

### Properties

|Name|Type|Required|Restrictions|Description|
|---|---|---|---|---|
|pattern|string|false|none|Doublestar pattern, matched against the full ref, name:tag, and the bare tag.|
|ref|string|false|none|Exact image reference to pin or unpin.|

<h2 id="tocS_server.readyzResponse">server.readyzResponse</h2>
<!-- backwards compatibility -->
<a id="schemaserver.readyzresponse"></a>
<a id="schema_server.readyzResponse"></a>
<a id="tocSserver.readyzresponse"></a>
<a id="tocsserver.readyzresponse"></a>

```json
{
  "ready": true,
  "stores": [
    {
      "cached": true,
      "checked_at": "string",
      "error": "string",
      "healthy": true,
      "kind": "string",
      "latency_ms": 0,
      "name": "string"
    }
  ]
}

```

### Properties

|Name|Type|Required|Restrictions|Description|
|---|---|---|---|---|
|ready|boolean|false|none|none|
|stores|[[health.Report](#schemahealth.report)]|false|none|none|

<h2 id="tocS_server.removeRequest">server.removeRequest</h2>
<!-- backwards compatibility -->
<a id="schemaserver.removerequest"></a>
<a id="schema_server.removeRequest"></a>
<a id="tocSserver.removerequest"></a>
<a id="tocsserver.removerequest"></a>

```json
{
  "ref": "docker.io/library/nginx:1.27"
}

```

### Properties

|Name|Type|Required|Restrictions|Description|
|---|---|---|---|---|
|ref|string|true|none|Image reference to delete from the engine store (required).|

<h2 id="tocS_server.storeListResponse">server.storeListResponse</h2>
<!-- backwards compatibility -->
<a id="schemaserver.storelistresponse"></a>
<a id="schema_server.storeListResponse"></a>
<a id="tocSserver.storelistresponse"></a>
<a id="tocsserver.storelistresponse"></a>

```json
{
  "items": [
    {
      "address": "string",
      "capabilities": {
        "gc": true,
        "pull": true,
        "read": true,
        "verify": true,
        "write": true
      },
      "error": "string",
      "host": "string",
      "kind": "oci",
      "mode": "string",
      "name": "string",
      "namespace": "string",
      "ready": true
    }
  ]
}

```

### Properties

|Name|Type|Required|Restrictions|Description|
|---|---|---|---|---|
|items|[[store.Status](#schemastore.status)]|false|none|none|

<h2 id="tocS_server.storePullRequest">server.storePullRequest</h2>
<!-- backwards compatibility -->
<a id="schemaserver.storepullrequest"></a>
<a id="schema_server.storePullRequest"></a>
<a id="tocSserver.storepullrequest"></a>
<a id="tocsserver.storepullrequest"></a>

```json
{
  "ref": "docker.io/library/nginx:1.27"
}

```

### Properties

|Name|Type|Required|Restrictions|Description|
|---|---|---|---|---|
|ref|string|true|none|Image reference the engine store should pull (required).|

<h2 id="tocS_server.storePullResponse">server.storePullResponse</h2>
<!-- backwards compatibility -->
<a id="schemaserver.storepullresponse"></a>
<a id="schema_server.storePullResponse"></a>
<a id="tocSserver.storepullresponse"></a>
<a id="tocsserver.storepullresponse"></a>

```json
{
  "kind": "string",
  "ref": "string",
  "state": "string",
  "store": "string"
}

```

### Properties

|Name|Type|Required|Restrictions|Description|
|---|---|---|---|---|
|kind|string|false|none|none|
|ref|string|false|none|none|
|state|string|false|none|none|
|store|string|false|none|none|

<h2 id="tocS_server.verifyDescribeResponse">server.verifyDescribeResponse</h2>
<!-- backwards compatibility -->
<a id="schemaserver.verifydescriberesponse"></a>
<a id="schema_server.verifyDescribeResponse"></a>
<a id="tocSserver.verifydescriberesponse"></a>
<a id="tocsserver.verifydescriberesponse"></a>

```json
{
  "anchors": [
    {
      "fingerprint": "string",
      "not_after": "string",
      "subject": "string"
    }
  ],
  "enabled": true,
  "level": "string",
  "mode": "string",
  "policies": [
    {
      "name": "string",
      "registry_scopes": [
        "string"
      ],
      "trusted_identities": [
        "string"
      ],
      "verification_level": "string"
    }
  ],
  "provider": "string",
  "stores": {
    "property1": "string",
    "property2": "string"
  }
}

```

### Properties

|Name|Type|Required|Restrictions|Description|
|---|---|---|---|---|
|anchors|[[verify.AnchorInfo](#schemaverify.anchorinfo)]|false|none|none|
|enabled|boolean|false|none|none|
|level|string|false|none|none|
|mode|string|false|none|global default: off | verify-if-present | require|
|policies|[[verify.PolicyInfo](#schemaverify.policyinfo)]|false|none|none|
|provider|string|false|none|none|
|stores|object|false|none|Stores maps each source registry store to its effective mode (per-store<br>override or the global default).|
|» **additionalProperties**|string|false|none|none|

<h2 id="tocS_server.verifyRequest">server.verifyRequest</h2>
<!-- backwards compatibility -->
<a id="schemaserver.verifyrequest"></a>
<a id="schema_server.verifyRequest"></a>
<a id="tocSserver.verifyrequest"></a>
<a id="tocsserver.verifyrequest"></a>

```json
{
  "from": "dockerhub",
  "ref": "docker.io/library/nginx:1.27"
}

```

### Properties

|Name|Type|Required|Restrictions|Description|
|---|---|---|---|---|
|from|string|false|none|Source registry store name or host; defaults to the ref's registry.|
|ref|string|true|none|Image reference to verify (required).|

<h2 id="tocS_server.verifyResponse">server.verifyResponse</h2>
<!-- backwards compatibility -->
<a id="schemaserver.verifyresponse"></a>
<a id="schema_server.verifyResponse"></a>
<a id="tocSserver.verifyresponse"></a>
<a id="tocsserver.verifyresponse"></a>

```json
{
  "digest": "string",
  "mode": "off",
  "ref": "string",
  "verified": true
}

```

### Properties

|Name|Type|Required|Restrictions|Description|
|---|---|---|---|---|
|digest|string|false|none|the verified digest|
|mode|string|false|none|effective mode for the source store|
|ref|string|false|none|the resolved source reference that was checked|
|verified|boolean|false|none|a signature was verified|

#### Enumerated Values

|Property|Value|
|---|---|
|mode|off|
|mode|verify-if-present|
|mode|require|

<h2 id="tocS_server.versionResponse">server.versionResponse</h2>
<!-- backwards compatibility -->
<a id="schemaserver.versionresponse"></a>
<a id="schema_server.versionResponse"></a>
<a id="tocSserver.versionresponse"></a>
<a id="tocsserver.versionresponse"></a>

```json
{
  "git_dirty": true,
  "git_rev": "string",
  "version": "string"
}

```

### Properties

|Name|Type|Required|Restrictions|Description|
|---|---|---|---|---|
|git_dirty|boolean|false|none|none|
|git_rev|string|false|none|none|
|version|string|false|none|none|

<h2 id="tocS_store.Caps">store.Caps</h2>
<!-- backwards compatibility -->
<a id="schemastore.caps"></a>
<a id="schema_store.Caps"></a>
<a id="tocSstore.caps"></a>
<a id="tocsstore.caps"></a>

```json
{
  "gc": true,
  "pull": true,
  "read": true,
  "verify": true,
  "write": true
}

```

what this store can do

### Properties

|Name|Type|Required|Restrictions|Description|
|---|---|---|---|---|
|gc|boolean|false|none|engine supports image GC (phase 2)|
|pull|boolean|false|none|engine can be triggered to pull|
|read|boolean|false|none|registry: pull blobs|
|verify|boolean|false|none|engine can verify signatures (phase 2)|
|write|boolean|false|none|registry: push blobs|

<h2 id="tocS_store.Status">store.Status</h2>
<!-- backwards compatibility -->
<a id="schemastore.status"></a>
<a id="schema_store.Status"></a>
<a id="tocSstore.status"></a>
<a id="tocsstore.status"></a>

```json
{
  "address": "string",
  "capabilities": {
    "gc": true,
    "pull": true,
    "read": true,
    "verify": true,
    "write": true
  },
  "error": "string",
  "host": "string",
  "kind": "oci",
  "mode": "string",
  "name": "string",
  "namespace": "string",
  "ready": true
}

```

### Properties

|Name|Type|Required|Restrictions|Description|
|---|---|---|---|---|
|address|string|false|none|none|
|capabilities|[store.Caps](#schemastore.caps)|false|none|what this store can do|
|error|string|false|none|engine readiness error, if not ready|
|host|string|false|none|none|
|kind|string|false|none|none|
|mode|string|false|none|none|
|name|string|false|none|none|
|namespace|string|false|none|none|
|ready|boolean|false|none|registries: always true (from config); engines: live Ready() probe|

#### Enumerated Values

|Property|Value|
|---|---|
|kind|oci|
|kind|docker|
|kind|containerd|

<h2 id="tocS_verify.AnchorInfo">verify.AnchorInfo</h2>
<!-- backwards compatibility -->
<a id="schemaverify.anchorinfo"></a>
<a id="schema_verify.AnchorInfo"></a>
<a id="tocSverify.anchorinfo"></a>
<a id="tocsverify.anchorinfo"></a>

```json
{
  "fingerprint": "string",
  "not_after": "string",
  "subject": "string"
}

```

### Properties

|Name|Type|Required|Restrictions|Description|
|---|---|---|---|---|
|fingerprint|string|false|none|sha256 over the DER encoding|
|not_after|string|false|none|none|
|subject|string|false|none|none|

<h2 id="tocS_verify.Description">verify.Description</h2>
<!-- backwards compatibility -->
<a id="schemaverify.description"></a>
<a id="schema_verify.Description"></a>
<a id="tocSverify.description"></a>
<a id="tocsverify.description"></a>

```json
{
  "anchors": [
    {
      "fingerprint": "string",
      "not_after": "string",
      "subject": "string"
    }
  ],
  "enabled": true,
  "level": "string",
  "mode": "string",
  "policies": [
    {
      "name": "string",
      "registry_scopes": [
        "string"
      ],
      "trusted_identities": [
        "string"
      ],
      "verification_level": "string"
    }
  ],
  "provider": "string"
}

```

### Properties

|Name|Type|Required|Restrictions|Description|
|---|---|---|---|---|
|anchors|[[verify.AnchorInfo](#schemaverify.anchorinfo)]|false|none|none|
|enabled|boolean|false|none|none|
|level|string|false|none|none|
|mode|string|false|none|global default: off | verify-if-present | require|
|policies|[[verify.PolicyInfo](#schemaverify.policyinfo)]|false|none|none|
|provider|string|false|none|none|

<h2 id="tocS_verify.PolicyInfo">verify.PolicyInfo</h2>
<!-- backwards compatibility -->
<a id="schemaverify.policyinfo"></a>
<a id="schema_verify.PolicyInfo"></a>
<a id="tocSverify.policyinfo"></a>
<a id="tocsverify.policyinfo"></a>

```json
{
  "name": "string",
  "registry_scopes": [
    "string"
  ],
  "trusted_identities": [
    "string"
  ],
  "verification_level": "string"
}

```

### Properties

|Name|Type|Required|Restrictions|Description|
|---|---|---|---|---|
|name|string|false|none|none|
|registry_scopes|[string]|false|none|none|
|trusted_identities|[string]|false|none|none|
|verification_level|string|false|none|none|

<h2 id="tocS_warm.JobSnapshot">warm.JobSnapshot</h2>
<!-- backwards compatibility -->
<a id="schemawarm.jobsnapshot"></a>
<a id="schema_warm.JobSnapshot"></a>
<a id="tocSwarm.jobsnapshot"></a>
<a id="tocswarm.jobsnapshot"></a>

```json
{
  "created_at": "string",
  "ended_at": "string",
  "error": "string",
  "id": "string",
  "platforms": [
    "string"
  ],
  "ref": "string",
  "started_at": "string",
  "state": "pending",
  "transfers": [
    {
      "bytes_done": 0,
      "bytes_total": 0,
      "digest": "string",
      "error": "string",
      "from": "string",
      "kind": "oci",
      "layers": [
        {
          "digest": "string",
          "done": 0,
          "platform": "string",
          "state": "pending",
          "total": 0
        }
      ],
      "ref": "string",
      "state": "pending",
      "store": "string"
    }
  ],
  "verification": {
    "digest": "string",
    "mode": "off",
    "verified": true
  }
}

```

### Properties

|Name|Type|Required|Restrictions|Description|
|---|---|---|---|---|
|created_at|string|false|none|none|
|ended_at|string|false|none|none|
|error|string|false|none|none|
|id|string|false|none|none|
|platforms|[string]|false|none|none|
|ref|string|false|none|none|
|started_at|string|false|none|none|
|state|[warm.JobState](#schemawarm.jobstate)|false|none|none|
|transfers|[[warm.TransferSnapshot](#schemawarm.transfersnapshot)]|false|none|none|
|verification|[warm.VerificationSnapshot](#schemawarm.verificationsnapshot)|false|none|none|

<h2 id="tocS_warm.JobState">warm.JobState</h2>
<!-- backwards compatibility -->
<a id="schemawarm.jobstate"></a>
<a id="schema_warm.JobState"></a>
<a id="tocSwarm.jobstate"></a>
<a id="tocswarm.jobstate"></a>

```json
"pending"

```

### Properties

|Name|Type|Required|Restrictions|Description|
|---|---|---|---|---|
|*anonymous*|string|false|none|none|

#### Enumerated Values

|Property|Value|
|---|---|
|*anonymous*|pending|
|*anonymous*|running|
|*anonymous*|done|
|*anonymous*|failed|
|*anonymous*|canceled|

<h2 id="tocS_warm.LayerSnapshot">warm.LayerSnapshot</h2>
<!-- backwards compatibility -->
<a id="schemawarm.layersnapshot"></a>
<a id="schema_warm.LayerSnapshot"></a>
<a id="tocSwarm.layersnapshot"></a>
<a id="tocswarm.layersnapshot"></a>

```json
{
  "digest": "string",
  "done": 0,
  "platform": "string",
  "state": "pending",
  "total": 0
}

```

### Properties

|Name|Type|Required|Restrictions|Description|
|---|---|---|---|---|
|digest|string|false|none|none|
|done|integer|false|none|none|
|platform|string|false|none|none|
|state|string|false|none|per-layer progress state|
|total|integer|false|none|none|

#### Enumerated Values

|Property|Value|
|---|---|
|state|pending|
|state|pulling|
|state|done|
|state|exists|
|state|failed|

<h2 id="tocS_warm.TransferSnapshot">warm.TransferSnapshot</h2>
<!-- backwards compatibility -->
<a id="schemawarm.transfersnapshot"></a>
<a id="schema_warm.TransferSnapshot"></a>
<a id="tocSwarm.transfersnapshot"></a>
<a id="tocswarm.transfersnapshot"></a>

```json
{
  "bytes_done": 0,
  "bytes_total": 0,
  "digest": "string",
  "error": "string",
  "from": "string",
  "kind": "oci",
  "layers": [
    {
      "digest": "string",
      "done": 0,
      "platform": "string",
      "state": "pending",
      "total": 0
    }
  ],
  "ref": "string",
  "state": "pending",
  "store": "string"
}

```

### Properties

|Name|Type|Required|Restrictions|Description|
|---|---|---|---|---|
|bytes_done|integer|false|none|none|
|bytes_total|integer|false|none|none|
|digest|string|false|none|manifest/index digest the step was anchored to|
|error|string|false|none|none|
|from|string|false|none|none|
|kind|string|false|none|which store kind ran this step|
|layers|[[warm.LayerSnapshot](#schemawarm.layersnapshot)]|false|none|none|
|ref|string|false|none|none|
|state|string|false|none|transfer step state|
|store|string|false|none|none|

#### Enumerated Values

|Property|Value|
|---|---|
|kind|oci|
|kind|docker|
|kind|containerd|
|state|pending|
|state|running|
|state|done|
|state|exists|
|state|failed|

<h2 id="tocS_warm.VerificationSnapshot">warm.VerificationSnapshot</h2>
<!-- backwards compatibility -->
<a id="schemawarm.verificationsnapshot"></a>
<a id="schema_warm.VerificationSnapshot"></a>
<a id="tocSwarm.verificationsnapshot"></a>
<a id="tocswarm.verificationsnapshot"></a>

```json
{
  "digest": "string",
  "mode": "off",
  "verified": true
}

```

### Properties

|Name|Type|Required|Restrictions|Description|
|---|---|---|---|---|
|digest|string|false|none|the digest the job was pinned to|
|mode|string|false|none|effective mode for the source store|
|verified|boolean|false|none|a signature was actually verified|

#### Enumerated Values

|Property|Value|
|---|---|
|mode|off|
|mode|verify-if-present|
|mode|require|

